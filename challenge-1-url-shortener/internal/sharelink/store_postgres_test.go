package sharelink

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Run with a disposable database that has migrations/001_share_links.sql applied:
//
//	TEST_DATABASE_URL=postgres://youpass:youpass@localhost:5432/youpass go test ./...
func newPostgresStore(t *testing.T) *PostgresStore {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `TRUNCATE share_links, share_view_flushes`); err != nil {
		t.Fatal(err)
	}
	return NewPostgresStore(pool)
}

func TestPostgresStore(t *testing.T) {
	s := newPostgresStore(t)
	ctx := context.Background()

	l := &Link{Code: "AAAAAAAA", SubmissionID: 42, OwnerID: 7, Status: StatusActive, Version: 1}
	if err := s.Insert(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(ctx, &Link{Code: "AAAAAAAA", SubmissionID: 43, OwnerID: 7, Status: StatusActive, Version: 1}); !errors.Is(err, ErrCodeTaken) {
		t.Fatalf("duplicate code: got %v, want ErrCodeTaken", err)
	}
	if err := s.Insert(ctx, &Link{Code: "BBBBBBBB", SubmissionID: 42, OwnerID: 7, Status: StatusActive, Version: 1}); !errors.Is(err, ErrLinkExists) {
		t.Fatalf("second live link: got %v, want ErrLinkExists", err)
	}

	got, err := s.UpdateStatus(ctx, "AAAAAAAA", StatusDisabled)
	if err != nil || got.Status != StatusDisabled || got.Version != 2 {
		t.Fatalf("disable: %+v %v", got, err)
	}
	if live, err := s.FindLiveBySubmission(ctx, 42); err != nil || live.Code != "AAAAAAAA" {
		t.Fatalf("disabled link is still live: %+v %v", live, err)
	}

	// Concurrent flushes of the same batch apply it exactly once.
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.ApplyViewDeltas(ctx, "batch-1", map[string]int64{"AAAAAAAA": 10, "ZZZZZZZZ": 1}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got, _ := s.FindByCode(ctx, "AAAAAAAA"); got.ViewCount != 10 {
		t.Fatalf("view_count = %d, want 10", got.ViewCount)
	}

	if _, err := s.UpdateStatus(ctx, "AAAAAAAA", StatusDeleted); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateStatus(ctx, "AAAAAAAA", StatusActive); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted link was revived: %v", err)
	}
	if _, err := s.FindLiveBySubmission(ctx, 42); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted link is still live: %v", err)
	}
	// The tombstone frees the submission for a new link but keeps the code taken.
	if err := s.Insert(ctx, &Link{Code: "CCCCCCCC", SubmissionID: 42, OwnerID: 7, Status: StatusActive, Version: 1}); err != nil {
		t.Fatal(err)
	}
}
