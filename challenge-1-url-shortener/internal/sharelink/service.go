package sharelink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/singleflight"
)

const maxCodeAttempts = 5

type Service struct {
	store Store
	subs  Submissions
	cache *Cache
	sf    singleflight.Group
	log   *slog.Logger

	now         func() time.Time
	loadTimeout time.Duration
}

func NewService(store Store, subs Submissions, cache *Cache, log *slog.Logger) *Service {
	return &Service{
		store: store, subs: subs, cache: cache, log: log,
		now:         time.Now,
		loadTimeout: 2 * time.Second,
	}
}

type CreateOptions struct {
	ExpiresAt *time.Time
}

// Create returns the submission's share link, creating it if needed. It is
// idempotent: clicking "Share" twice returns the same URL. Sharing a
// submission whose link was turned off turns it back on, because the student
// is explicitly asking to share again.
func (s *Service) Create(ctx context.Context, userID, submissionID int64, opts CreateOptions) (link *Link, created bool, err error) {
	owner, err := s.subs.OwnerOf(ctx, submissionID)
	if err != nil {
		return nil, false, err
	}
	if owner != userID {
		return nil, false, ErrForbidden
	}

	existing, err := s.store.FindLiveBySubmission(ctx, submissionID)
	switch {
	case err == nil:
		if existing.Status == StatusDisabled {
			existing, err = s.setStatus(ctx, existing.Code, StatusActive)
		}
		return existing, false, err
	case !errors.Is(err, ErrNotFound):
		return nil, false, err
	}

	for range maxCodeAttempts {
		code, err := NewCode()
		if err != nil {
			return nil, false, err
		}
		l := &Link{
			Code: code, SubmissionID: submissionID, OwnerID: userID,
			Status: StatusActive, ExpiresAt: opts.ExpiresAt, Version: 1,
		}
		switch err := s.store.Insert(ctx, l); {
		case err == nil:
			// A negative-cache entry for this code, left by someone guessing
			// it, must not hide the new link.
			s.publish(ctx, l)
			return l, true, nil
		case errors.Is(err, ErrCodeTaken):
			continue
		case errors.Is(err, ErrLinkExists):
			// A concurrent request for the same submission won the race.
			l, err := s.store.FindLiveBySubmission(ctx, submissionID)
			return l, false, err
		default:
			return nil, false, err
		}
	}
	return nil, false, fmt.Errorf("sharelink: no free code after %d attempts", maxCodeAttempts)
}

// SetEnabled turns sharing on or off. The code is kept, so a student can
// share again without the old URL they already posted breaking.
func (s *Service) SetEnabled(ctx context.Context, userID int64, code string, enabled bool) (*Link, error) {
	l, err := s.ownedLink(ctx, userID, code)
	if err != nil {
		return nil, err
	}
	target := StatusDisabled
	if enabled {
		target = StatusActive
	}
	if l.Status == target {
		return l, nil
	}
	return s.setStatus(ctx, code, target)
}

// Delete permanently revokes a link. The row stays as a tombstone so the
// code is never reissued.
func (s *Service) Delete(ctx context.Context, userID int64, code string) error {
	if _, err := s.ownedLink(ctx, userID, code); err != nil {
		return err
	}
	_, err := s.setStatus(ctx, code, StatusDeleted)
	return err
}

// Get returns a link for its owner (management screens read the database
// directly, so they always see fresh data).
func (s *Service) Get(ctx context.Context, userID int64, code string) (*Link, error) {
	return s.ownedLink(ctx, userID, code)
}

// ResolvedLink is what the public endpoint needs to serve a shared submission.
type ResolvedLink struct {
	Code         string
	SubmissionID int64
	OwnerID      int64
}

// Resolve is the hot path behind https://youpass.vn/s/:code.
//
// Lookup order: L1 (in process, ~µs) → L2 (Redis, ~1ms) → Postgres
// (~ms, collapsed by singleflight). Access rules are checked on every
// request, never only when the cache is filled, so expiry needs no
// invalidation and a disabled link stops working as soon as its cache entry
// is replaced.
func (s *Service) Resolve(ctx context.Context, code string) (*ResolvedLink, error) {
	if !ValidCode(code) {
		return nil, ErrNotFound
	}
	e, ok := s.cache.Get(ctx, code)
	if !ok {
		v, err, _ := s.sf.Do(code, func() (any, error) {
			// Callers share this load, so one caller's cancellation must not
			// fail the others.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.loadTimeout)
			defer cancel()

			l, err := s.store.FindByCode(ctx, code)
			if errors.Is(err, ErrNotFound) {
				e := missingEntry(code)
				s.cache.Fill(ctx, e)
				return e, nil
			}
			if err != nil {
				return nil, err
			}
			e := toCached(l)
			s.cache.Fill(ctx, e)
			return e, nil
		})
		if err != nil {
			return nil, err
		}
		e = v.(*cachedLink)
	}
	if !e.accessible(s.now()) {
		return nil, ErrNotFound
	}
	return &ResolvedLink{Code: e.Code, SubmissionID: e.SubmissionID, OwnerID: e.OwnerID}, nil
}

// ownedLink hides other users' links behind ErrNotFound so the management
// API cannot be used to probe which codes exist.
func (s *Service) ownedLink(ctx context.Context, userID int64, code string) (*Link, error) {
	if !ValidCode(code) {
		return nil, ErrNotFound
	}
	l, err := s.store.FindByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	if l.OwnerID != userID || l.Status == StatusDeleted {
		return nil, ErrNotFound
	}
	return l, nil
}

func (s *Service) setStatus(ctx context.Context, code string, status Status) (*Link, error) {
	l, err := s.store.UpdateStatus(ctx, code, status)
	if err != nil {
		return nil, err
	}
	s.publish(ctx, l)
	return l, nil
}

// publish pushes a committed change to the cache. The database is already
// updated, so the request succeeds either way; if Redis is unreachable we
// keep retrying in the background so a revoked link cannot stay public for
// the full Redis TTL.
func (s *Service) publish(ctx context.Context, l *Link) {
	e := toCached(l)
	if err := s.cache.Publish(ctx, e); err == nil {
		return
	}
	s.log.ErrorContext(ctx, "sharelink: cache publish failed, retrying in background", "code", l.Code)
	go func() {
		backoff := time.Second
		for deadline := time.Now().Add(s.cache.cfg.RedisTTL); time.Now().Before(deadline); {
			time.Sleep(backoff)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := s.cache.Publish(ctx, e)
			cancel()
			if err == nil {
				return
			}
			backoff = min(backoff*2, 30*time.Second)
		}
	}()
}
