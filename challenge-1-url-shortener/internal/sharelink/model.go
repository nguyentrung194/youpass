package sharelink

import (
	"context"
	"errors"
	"time"
)

type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled" // owner turned sharing off; can be re-enabled with the same code
	StatusDeleted  Status = "deleted"  // permanent tombstone; the code is never reused

	// statusMissing only exists in the cache, to remember that a code has no
	// row (negative caching against scans and typos).
	statusMissing Status = "missing"
)

var (
	// ErrNotFound is returned for any link the caller may not see. Missing,
	// disabled, deleted, expired and "not yours" all map to it so responses
	// never reveal whether a code exists.
	ErrNotFound           = errors.New("sharelink: not found")
	ErrForbidden          = errors.New("sharelink: forbidden")
	ErrSubmissionNotFound = errors.New("sharelink: submission not found")

	// Store-level conflicts.
	ErrCodeTaken  = errors.New("sharelink: code already taken")
	ErrLinkExists = errors.New("sharelink: submission already has a live link")
)

type Link struct {
	Code         string
	SubmissionID int64
	OwnerID      int64
	Status       Status
	ExpiresAt    *time.Time
	ViewCount    int64
	Version      int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Accessible reports whether the public may open the link at now.
func (l *Link) Accessible(now time.Time) bool {
	return l.Status == StatusActive && (l.ExpiresAt == nil || now.Before(*l.ExpiresAt))
}

// Store is the source of truth for share links (Postgres in production).
type Store interface {
	// FindByCode returns the link in any status, or ErrNotFound.
	FindByCode(ctx context.Context, code string) (*Link, error)
	// FindLiveBySubmission returns the active or disabled link of a submission, or ErrNotFound.
	FindLiveBySubmission(ctx context.Context, submissionID int64) (*Link, error)
	// Insert returns ErrCodeTaken or ErrLinkExists on unique conflicts.
	Insert(ctx context.Context, l *Link) error
	// UpdateStatus changes the status of a non-deleted link, bumps its
	// version and returns the new row. It returns ErrNotFound if the link is
	// missing or already deleted.
	UpdateStatus(ctx context.Context, code string, status Status) (*Link, error)
	// ApplyViewDeltas adds view deltas in one transaction. It is idempotent
	// per batchID: re-applying a batch that was already committed is a no-op.
	ApplyViewDeltas(ctx context.Context, batchID string, deltas map[string]int64) error
}

// Submissions is the part of YouPass's submission service this feature needs.
type Submissions interface {
	// OwnerOf returns the student who owns the submission, or ErrSubmissionNotFound.
	OwnerOf(ctx context.Context, submissionID int64) (int64, error)
	// PublicView returns the read-only data a visitor may see (score,
	// feedback, answer), without private fields such as email or phone.
	PublicView(ctx context.Context, submissionID int64) (any, error)
}
