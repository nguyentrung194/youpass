package sharelink

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// MemoryStore is an in-process Store for tests and local demos.
type MemoryStore struct {
	mu      sync.Mutex
	links   map[string]*Link
	batches map[string]bool

	// FindByCodeCalls counts database reads, so tests can assert that the
	// cache and singleflight actually shield the database.
	FindByCodeCalls atomic.Int64
	// FindByCodeDelay simulates query latency.
	FindByCodeDelay time.Duration
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{links: map[string]*Link{}, batches: map[string]bool{}}
}

func clone(l *Link) *Link {
	c := *l
	return &c
}

func (s *MemoryStore) FindByCode(_ context.Context, code string) (*Link, error) {
	s.FindByCodeCalls.Add(1)
	time.Sleep(s.FindByCodeDelay)
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[code]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(l), nil
}

func (s *MemoryStore) FindLiveBySubmission(_ context.Context, submissionID int64) (*Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.links {
		if l.SubmissionID == submissionID && l.Status != StatusDeleted {
			return clone(l), nil
		}
	}
	return nil, ErrNotFound
}

func (s *MemoryStore) Insert(_ context.Context, l *Link) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.links[l.Code]; ok {
		return ErrCodeTaken
	}
	for _, other := range s.links {
		if other.SubmissionID == l.SubmissionID && other.Status != StatusDeleted {
			return ErrLinkExists
		}
	}
	now := time.Now()
	l.CreatedAt, l.UpdatedAt = now, now
	s.links[l.Code] = clone(l)
	return nil
}

func (s *MemoryStore) UpdateStatus(_ context.Context, code string, status Status) (*Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[code]
	if !ok || l.Status == StatusDeleted {
		return nil, ErrNotFound
	}
	l.Status = status
	l.Version++
	l.UpdatedAt = time.Now()
	return clone(l), nil
}

func (s *MemoryStore) ApplyViewDeltas(_ context.Context, batchID string, deltas map[string]int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.batches[batchID] {
		return nil
	}
	s.batches[batchID] = true
	for code, n := range deltas {
		if l, ok := s.links[code]; ok {
			l.ViewCount += n
		}
	}
	return nil
}
