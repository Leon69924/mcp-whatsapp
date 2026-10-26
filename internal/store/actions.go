package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Lifecycle of a queued action: pending -> (rejected | expired | executing),
// executing -> (executed | failed). Only the pending -> executing transition
// runs the action, and it is a single guarded UPDATE, so an action runs at
// most once.
const (
	ActionPending   = "pending"
	ActionRejected  = "rejected"
	ActionExpired   = "expired"
	ActionExecuting = "executing"
	ActionExecuted  = "executed"
	ActionFailed    = "failed"
)

var (
	// ErrActionNotFound is returned for an unknown action ID.
	ErrActionNotFound = errors.New("action: not found")
	// ErrActionNotPending is returned when a decision targets an action that
	// is no longer pending (already decided, executing or finished).
	ErrActionNotPending = errors.New("action: not pending")
	// ErrActionExpired is returned when a decision targets a pending action
	// whose approval window has closed.
	ErrActionExpired = errors.New("action: expired")
)

// PendingAction is a mutating tool call held for operator approval.
type PendingAction struct {
	ID         string
	Tool       string
	ArgsJSON   string
	Summary    string
	TargetJID  string
	Status     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	DecidedAt  time.Time // zero while the action is undecided
	ResultJSON string
}

const actionColumns = "id, tool, args_json, summary, target_jid, status, created_at, expires_at, decided_at, result_json"

func scanAction(row interface {
	Scan(dest ...any) error
}) (PendingAction, error) {
	var (
		a         PendingAction
		decidedAt sql.NullTime
		result    sql.NullString
	)
	if err := row.Scan(&a.ID, &a.Tool, &a.ArgsJSON, &a.Summary, &a.TargetJID, &a.Status, &a.CreatedAt, &a.ExpiresAt, &decidedAt, &result); err != nil {
		return PendingAction{}, err
	}
	a.DecidedAt = decidedAt.Time
	a.ResultJSON = result.String
	return a, nil
}

// CreateAction persists a new pending action.
func (s *Store) CreateAction(ctx context.Context, a PendingAction) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pending_actions (id, tool, args_json, summary, target_jid, status, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Tool, a.ArgsJSON, a.Summary, a.TargetJID, ActionPending, a.CreatedAt.UTC(), a.ExpiresAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("create action: %w", err)
	}
	return nil
}

// GetAction returns one action, or ErrActionNotFound.
func (s *Store) GetAction(ctx context.Context, id string) (PendingAction, error) {
	a, err := scanAction(s.db.QueryRowContext(ctx,
		"SELECT "+actionColumns+" FROM pending_actions WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return PendingAction{}, ErrActionNotFound
	}
	if err != nil {
		return PendingAction{}, fmt.Errorf("get action: %w", err)
	}
	return a, nil
}

// ListPendingActions returns the actions still awaiting a decision at now,
// oldest first.
func (s *Store) ListPendingActions(ctx context.Context, now time.Time) ([]PendingAction, error) {
	return s.queryActions(ctx,
		"SELECT "+actionColumns+" FROM pending_actions WHERE status = ? AND expires_at > ? ORDER BY created_at",
		ActionPending, now.UTC())
}

// ListDecidedActions returns the most recently decided actions (every status
// other than pending), newest first.
func (s *Store) ListDecidedActions(ctx context.Context, limit int) ([]PendingAction, error) {
	return s.queryActions(ctx,
		"SELECT "+actionColumns+" FROM pending_actions WHERE status != ? ORDER BY decided_at DESC, created_at DESC LIMIT ?",
		ActionPending, limit)
}

func (s *Store) queryActions(ctx context.Context, query string, args ...any) ([]PendingAction, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list actions: %w", err)
	}
	defer rows.Close()
	var actions []PendingAction
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, fmt.Errorf("scan action: %w", err)
		}
		actions = append(actions, a)
	}
	return actions, rows.Err()
}

// ExpireActions marks every pending action whose window has closed at now as
// expired, and returns how many it changed.
func (s *Store) ExpireActions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE pending_actions SET status = ?, decided_at = expires_at WHERE status = ? AND expires_at <= ?",
		ActionExpired, ActionPending, now.UTC())
	if err != nil {
		return 0, fmt.Errorf("expire actions: %w", err)
	}
	return res.RowsAffected()
}

// ClaimAction atomically moves a pending, unexpired action to executing and
// returns it. Exactly one concurrent caller wins; every other caller gets an
// error naming why the action cannot run.
func (s *Store) ClaimAction(ctx context.Context, id string, now time.Time) (PendingAction, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE pending_actions SET status = ?, decided_at = ? WHERE id = ? AND status = ? AND expires_at > ?",
		ActionExecuting, now.UTC(), id, ActionPending, now.UTC())
	if err != nil {
		return PendingAction{}, fmt.Errorf("claim action: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return PendingAction{}, fmt.Errorf("claim action: %w", err)
	}
	if n == 1 {
		return s.GetAction(ctx, id)
	}
	return PendingAction{}, s.whyNotPending(ctx, id, now)
}

// RejectAction moves a pending action to rejected.
func (s *Store) RejectAction(ctx context.Context, id string, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE pending_actions SET status = ?, decided_at = ? WHERE id = ? AND status = ? AND expires_at > ?",
		ActionRejected, now.UTC(), id, ActionPending, now.UTC())
	if err != nil {
		return fmt.Errorf("reject action: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("reject action: %w", err)
	}
	if n == 1 {
		return nil
	}
	return s.whyNotPending(ctx, id, now)
}

// whyNotPending explains a refused decision, expiring the action when its
// window had closed without anyone noticing yet.
func (s *Store) whyNotPending(ctx context.Context, id string, now time.Time) error {
	a, err := s.GetAction(ctx, id)
	if err != nil {
		return err
	}
	if a.Status == ActionPending {
		if _, err := s.ExpireActions(ctx, now); err != nil {
			return err
		}
		return ErrActionExpired
	}
	return fmt.Errorf("%w (status %s)", ErrActionNotPending, a.Status)
}

// FinishAction records the outcome of an executing action. status must be
// ActionExecuted or ActionFailed.
func (s *Store) FinishAction(ctx context.Context, id, status, resultJSON string) error {
	if status != ActionExecuted && status != ActionFailed {
		return fmt.Errorf("finish action: status must be %q or %q, got %q", ActionExecuted, ActionFailed, status)
	}
	res, err := s.db.ExecContext(ctx,
		"UPDATE pending_actions SET status = ?, result_json = ? WHERE id = ? AND status = ?",
		status, resultJSON, id, ActionExecuting)
	if err != nil {
		return fmt.Errorf("finish action: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finish action: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("finish action %s: not executing", id)
	}
	return nil
}
