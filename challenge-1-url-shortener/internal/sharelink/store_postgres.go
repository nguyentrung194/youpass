package sharelink

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	pgUniqueViolation     = "23505"
	pkConstraint          = "share_links_pkey"
	submissionLiveConstrt = "share_links_submission_live_idx"

	linkColumns = `code, submission_id, owner_id, status, expires_at, view_count, version, created_at, updated_at`
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func scanLink(row pgx.Row) (*Link, error) {
	var l Link
	err := row.Scan(&l.Code, &l.SubmissionID, &l.OwnerID, &l.Status, &l.ExpiresAt,
		&l.ViewCount, &l.Version, &l.CreatedAt, &l.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func (s *PostgresStore) FindByCode(ctx context.Context, code string) (*Link, error) {
	return scanLink(s.pool.QueryRow(ctx,
		`SELECT `+linkColumns+` FROM share_links WHERE code = $1`, code))
}

func (s *PostgresStore) FindLiveBySubmission(ctx context.Context, submissionID int64) (*Link, error) {
	return scanLink(s.pool.QueryRow(ctx,
		`SELECT `+linkColumns+` FROM share_links WHERE submission_id = $1 AND status <> 'deleted'`,
		submissionID))
}

func (s *PostgresStore) Insert(ctx context.Context, l *Link) error {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO share_links (code, submission_id, owner_id, status, expires_at, version)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at, updated_at`,
		l.Code, l.SubmissionID, l.OwnerID, l.Status, l.ExpiresAt, l.Version)
	err := row.Scan(&l.CreatedAt, &l.UpdatedAt)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		switch pgErr.ConstraintName {
		case pkConstraint:
			return ErrCodeTaken
		case submissionLiveConstrt:
			return ErrLinkExists
		}
	}
	return err
}

func (s *PostgresStore) UpdateStatus(ctx context.Context, code string, status Status) (*Link, error) {
	// The status guard makes deletion final even under concurrent requests.
	return scanLink(s.pool.QueryRow(ctx, `
		UPDATE share_links
		SET status = $2, version = version + 1, updated_at = now()
		WHERE code = $1 AND status <> 'deleted'
		RETURNING `+linkColumns, code, status))
}

func (s *PostgresStore) ApplyViewDeltas(ctx context.Context, batchID string, deltas map[string]int64) error {
	codes := make([]string, 0, len(deltas))
	counts := make([]int64, 0, len(deltas))
	for code, n := range deltas {
		codes = append(codes, code)
		counts = append(counts, n)
	}

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Claim the batch first. A concurrent flusher holding the same batch
		// blocks on the unique index until we commit, then sees the conflict.
		tag, err := tx.Exec(ctx,
			`INSERT INTO share_view_flushes (batch_id) VALUES ($1) ON CONFLICT DO NOTHING`, batchID)
		if err != nil {
			return fmt.Errorf("claim view batch: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil // already applied
		}
		_, err = tx.Exec(ctx, `
			UPDATE share_links AS l
			SET view_count = l.view_count + d.delta
			FROM unnest($1::text[], $2::bigint[]) AS d(code, delta)
			WHERE l.code = d.code`, codes, counts)
		if err != nil {
			return fmt.Errorf("apply view deltas: %w", err)
		}
		return nil
	})
}
