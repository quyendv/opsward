package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/quyendv/opsward/internal/store"
)

const approvalColumns = `id, requester, tool, environment, namespace, kind, name, args_hash, diff, approver_users, approver_groups, status, created_ns, expires_ns, decided_by, decided_ns, reason, used_ns, rule, profile, decided_groups`

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// CreateApproval implements store.Store.
func (s *Store) CreateApproval(ctx context.Context, a store.Approval) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO approvals (`+approvalColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)`,
		a.ID, a.Requester, a.Tool, a.Environment, a.Namespace, a.Kind, a.Name, a.ArgsHash, a.Diff, nonNil(a.ApproverUsers), nonNil(a.ApproverGroups),
		a.Status, a.CreatedAt.UnixNano(), a.ExpiresAt.UnixNano(), a.DecidedBy, nanos(a.DecidedAt), a.Reason, nanos(a.UsedAt),
		a.Rule, a.Profile, nonNil(a.DecidedGroups))
	if err != nil {
		return fmt.Errorf("postgres: create approval: %w", err)
	}
	return nil
}

// Approval implements store.Store.
func (s *Store) Approval(ctx context.Context, id string) (store.Approval, bool, error) {
	a, err := scanApproval(s.pool.QueryRow(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Approval{}, false, nil
	}
	if err != nil {
		return store.Approval{}, false, fmt.Errorf("postgres: read approval: %w", err)
	}
	return a, true, nil
}

// ListApprovals implements store.Store.
func (s *Store) ListApprovals(ctx context.Context, since time.Time, limit int) ([]store.Approval, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE created_ns >= $1 ORDER BY created_ns DESC, id LIMIT $2`, since.UnixNano(), limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list approvals: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (store.Approval, error) { return scanApproval(r) })
	if err != nil {
		return nil, fmt.Errorf("postgres: list approvals: %w", err)
	}
	return out, nil
}

// CountPendingApprovals implements store.Store.
func (s *Store) CountPendingApprovals(ctx context.Context, requester string, now time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE requester = $1 AND status = $2 AND expires_ns > $3`,
		requester, store.ApprovalPending, now.UnixNano()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("postgres: count approvals: %w", err)
	}
	return n, nil
}

// PruneApprovals implements store.Store.
func (s *Store) PruneApprovals(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM approvals WHERE expires_ns < $1`, before.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("postgres: prune approvals: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DecideApproval implements store.Store.
func (s *Store) DecideApproval(ctx context.Context, id, status, by string, byGroups []string, reason string, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE approvals SET status = $1, decided_by = $2, decided_groups = $3, reason = $4, decided_ns = $5
		WHERE id = $6 AND status = $7 AND expires_ns > $5`,
		status, by, nonNil(byGroups), reason, at.UnixNano(), id, store.ApprovalPending)
	if err != nil {
		return false, fmt.Errorf("postgres: decide approval: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// WithdrawApproval implements store.Store.
func (s *Store) WithdrawApproval(ctx context.Context, id, by string, byGroups []string, reason string, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE approvals SET status = $1, decided_by = $2, decided_groups = $3, reason = $4, decided_ns = $5
		WHERE id = $6 AND status = $7 AND expires_ns > $5`,
		store.ApprovalDenied, by, nonNil(byGroups), reason, at.UnixNano(), id, store.ApprovalApproved)
	if err != nil {
		return false, fmt.Errorf("postgres: withdraw approval: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// UseApproval implements store.Store.
func (s *Store) UseApproval(ctx context.Context, id string, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE approvals SET status = $1, used_ns = $2 WHERE id = $3 AND status = $4 AND expires_ns > $2`,
		store.ApprovalUsed, at.UnixNano(), id, store.ApprovalApproved)
	if err != nil {
		return false, fmt.Errorf("postgres: use approval: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func scanApproval(row pgx.Row) (store.Approval, error) {
	var a store.Approval
	var created, expires, decided, used int64
	if err := row.Scan(&a.ID, &a.Requester, &a.Tool, &a.Environment, &a.Namespace, &a.Kind, &a.Name, &a.ArgsHash, &a.Diff,
		&a.ApproverUsers, &a.ApproverGroups, &a.Status, &created, &expires, &a.DecidedBy, &decided, &a.Reason, &used,
		&a.Rule, &a.Profile, &a.DecidedGroups); err != nil {
		return store.Approval{}, err
	}
	a.CreatedAt, a.ExpiresAt = time.Unix(0, created).UTC(), time.Unix(0, expires).UTC()
	a.DecidedAt, a.UsedAt = fromNanos(decided), fromNanos(used)
	return a, nil
}
