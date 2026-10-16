package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/quyendv/opsward/internal/store"
)

const approvalColumns = `id, requester, tool, environment, namespace, kind, name, args_hash, diff, approver_users, approver_groups, status, created_ns, expires_ns, decided_by, decided_ns, reason, used_ns, rule, profile, decided_groups`

// CreateApproval implements store.Store.
func (s *Store) CreateApproval(ctx context.Context, a store.Approval) error {
	users, _ := json.Marshal(nonNil(a.ApproverUsers))
	groups, _ := json.Marshal(nonNil(a.ApproverGroups))
	decided, _ := json.Marshal(nonNil(a.DecidedGroups))
	_, err := s.db.ExecContext(ctx, `INSERT INTO approvals (`+approvalColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Requester, a.Tool, a.Environment, a.Namespace, a.Kind, a.Name, a.ArgsHash, a.Diff, string(users), string(groups),
		a.Status, a.CreatedAt.UnixNano(), a.ExpiresAt.UnixNano(), a.DecidedBy, nanos(a.DecidedAt), a.Reason, nanos(a.UsedAt),
		a.Rule, a.Profile, string(decided))
	if err != nil {
		return fmt.Errorf("sqlite: create approval: %w", err)
	}
	return nil
}

// Approval implements store.Store.
func (s *Store) Approval(ctx context.Context, id string) (store.Approval, bool, error) {
	a, err := scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Approval{}, false, nil
	}
	if err != nil {
		return store.Approval{}, false, fmt.Errorf("sqlite: read approval: %w", err)
	}
	return a, true, nil
}

// ListApprovals implements store.Store.
func (s *Store) ListApprovals(ctx context.Context, since time.Time) ([]store.Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE created_ns >= ? ORDER BY created_ns DESC, id`, since.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("sqlite: list approvals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list approvals: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list approvals: %w", err)
	}
	return out, nil
}

// DecideApproval implements store.Store.
func (s *Store) DecideApproval(ctx context.Context, id, status, by string, byGroups []string, reason string, at time.Time) (bool, error) {
	groups, _ := json.Marshal(nonNil(byGroups))
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status = ?, decided_by = ?, decided_groups = ?, reason = ?, decided_ns = ?
		WHERE id = ? AND status = ? AND expires_ns > ?`,
		status, by, string(groups), reason, at.UnixNano(), id, store.ApprovalPending, at.UnixNano())
	if err != nil {
		return false, fmt.Errorf("sqlite: decide approval: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// UseApproval implements store.Store.
func (s *Store) UseApproval(ctx context.Context, id string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status = ?, used_ns = ? WHERE id = ? AND status = ? AND expires_ns > ?`,
		store.ApprovalUsed, at.UnixNano(), id, store.ApprovalApproved, at.UnixNano())
	if err != nil {
		return false, fmt.Errorf("sqlite: use approval: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func scanApproval(sc scanner) (store.Approval, error) {
	var a store.Approval
	var users, groups, decidedGroups string
	var created, expires, decided, used int64
	if err := sc.Scan(&a.ID, &a.Requester, &a.Tool, &a.Environment, &a.Namespace, &a.Kind, &a.Name, &a.ArgsHash, &a.Diff,
		&users, &groups, &a.Status, &created, &expires, &a.DecidedBy, &decided, &a.Reason, &used, &a.Rule, &a.Profile, &decidedGroups); err != nil {
		return store.Approval{}, err
	}
	if err := json.Unmarshal([]byte(users), &a.ApproverUsers); err != nil {
		return store.Approval{}, err
	}
	if err := json.Unmarshal([]byte(groups), &a.ApproverGroups); err != nil {
		return store.Approval{}, err
	}
	if err := json.Unmarshal([]byte(decidedGroups), &a.DecidedGroups); err != nil {
		return store.Approval{}, err
	}
	a.CreatedAt, a.ExpiresAt = time.Unix(0, created).UTC(), time.Unix(0, expires).UTC()
	a.DecidedAt, a.UsedAt = fromNanos(decided), fromNanos(used)
	return a, nil
}
