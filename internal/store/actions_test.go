package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var actionEpoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func newTestAction(t *testing.T, s *Store, id string, ttl time.Duration) PendingAction {
	t.Helper()
	a := PendingAction{
		ID:        id,
		Tool:      "send_message",
		ArgsJSON:  `{"recipient":"447700000001","message":"hi"}`,
		Summary:   `send_message to 447700000001@s.whatsapp.net: "hi"`,
		TargetJID: "447700000001@s.whatsapp.net",
		CreatedAt: actionEpoch,
		ExpiresAt: actionEpoch.Add(ttl),
	}
	if err := s.CreateAction(context.Background(), a); err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	return a
}

func TestAction_PendingApprovedExecutedExactlyOnce(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	newTestAction(t, s, "act1", time.Hour)

	got, err := s.GetAction(ctx, "act1")
	if err != nil || got.Status != ActionPending || !got.DecidedAt.IsZero() {
		t.Fatalf("new action = %+v, %v; want pending and undecided", got, err)
	}

	claimed, err := s.ClaimAction(ctx, "act1", actionEpoch.Add(time.Minute))
	if err != nil {
		t.Fatalf("ClaimAction: %v", err)
	}
	if claimed.Status != ActionExecuting || claimed.ArgsJSON != got.ArgsJSON || claimed.DecidedAt.IsZero() || claimed.DecidedVia != DecidedByOperator {
		t.Fatalf("claimed = %+v; want executing with the stored args, a decision time and the operator as decider", claimed)
	}

	// A second approval must not run the action again.
	if _, err := s.ClaimAction(ctx, "act1", actionEpoch.Add(2*time.Minute)); !errors.Is(err, ErrActionNotPending) {
		t.Fatalf("second ClaimAction error = %v; want ErrActionNotPending", err)
	}

	if err := s.FinishAction(ctx, "act1", ActionExecuted, `{"is_error":false,"text":"ok"}`); err != nil {
		t.Fatalf("FinishAction: %v", err)
	}
	done, err := s.GetAction(ctx, "act1")
	if err != nil || done.Status != ActionExecuted || done.ResultJSON != `{"is_error":false,"text":"ok"}` {
		t.Fatalf("finished = %+v, %v", done, err)
	}
	if err := s.FinishAction(ctx, "act1", ActionFailed, "{}"); err == nil {
		t.Fatal("FinishAction on a finished action: want an error")
	}
	if _, err := s.ClaimAction(ctx, "act1", actionEpoch.Add(3*time.Minute)); !errors.Is(err, ErrActionNotPending) {
		t.Fatalf("ClaimAction after finish error = %v; want ErrActionNotPending", err)
	}
}

func TestAction_ConcurrentClaimsRunOnce(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	newTestAction(t, s, "act1", time.Hour)

	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.ClaimAction(ctx, "act1", actionEpoch.Add(time.Minute)); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := wins.Load(); got != 1 {
		t.Fatalf("%d concurrent claims won; want exactly 1", got)
	}
}

func TestAction_RejectedNeverExecutes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	newTestAction(t, s, "act1", time.Hour)

	if err := s.RejectAction(ctx, "act1", actionEpoch.Add(time.Minute)); err != nil {
		t.Fatalf("RejectAction: %v", err)
	}
	got, _ := s.GetAction(ctx, "act1")
	if got.Status != ActionRejected || got.DecidedVia != DecidedByOperator {
		t.Fatalf("action = %+v; want rejected by the operator", got)
	}
	if _, err := s.ClaimAction(ctx, "act1", actionEpoch.Add(2*time.Minute)); !errors.Is(err, ErrActionNotPending) {
		t.Fatalf("ClaimAction after reject error = %v; want ErrActionNotPending", err)
	}
	if err := s.RejectAction(ctx, "act1", actionEpoch.Add(2*time.Minute)); !errors.Is(err, ErrActionNotPending) {
		t.Fatalf("second RejectAction error = %v; want ErrActionNotPending", err)
	}
}

// An action decided in the MCP client is stored already decided, and its ID —
// the confirmation's nonce — can be recorded only once.
func TestAction_ClientDecisionsAreRecordedOnce(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	decided := actionEpoch.Add(time.Minute)
	base := PendingAction{
		Tool: "send_message", ArgsJSON: `{}`, Summary: "s", TargetJID: "447700000001@s.whatsapp.net",
		CreatedAt: actionEpoch, ExpiresAt: actionEpoch.Add(time.Hour),
	}
	for id, status := range map[string]string{"confirmed": ActionExecuting, "declined": ActionRejected} {
		a := base
		a.ID = id
		if err := s.CreateDecidedAction(ctx, a, status, decided); err != nil {
			t.Fatalf("CreateDecidedAction(%s): %v", id, err)
		}
		got, err := s.GetAction(ctx, id)
		if err != nil || got.Status != status || got.DecidedVia != DecidedInClient || !got.DecidedAt.Equal(decided) {
			t.Fatalf("%s = %+v, %v; want %s, decided in the client at %s", id, got, err, status, decided)
		}
		if err := s.CreateDecidedAction(ctx, a, status, decided); !errors.Is(err, ErrActionExists) {
			t.Fatalf("second CreateDecidedAction(%s) error = %v; want ErrActionExists", id, err)
		}
	}
	if err := s.FinishAction(ctx, "confirmed", ActionExecuted, `{"is_error":false,"text":"ok"}`); err != nil {
		t.Fatalf("FinishAction: %v", err)
	}
	if pending, err := s.ListPendingActions(ctx, decided); err != nil || len(pending) != 0 {
		t.Fatalf("client decisions surfaced as pending: %+v, %v", pending, err)
	}
	invalid := base
	invalid.ID = "pending"
	if err := s.CreateDecidedAction(ctx, invalid, ActionPending, decided); err == nil {
		t.Fatal("CreateDecidedAction with status pending: want an error")
	}
}

func TestAction_ExpiredNeverExecutes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	newTestAction(t, s, "act1", time.Hour)
	late := actionEpoch.Add(2 * time.Hour)

	if _, err := s.ClaimAction(ctx, "act1", late); !errors.Is(err, ErrActionExpired) {
		t.Fatalf("ClaimAction past expiry error = %v; want ErrActionExpired", err)
	}
	got, _ := s.GetAction(ctx, "act1")
	if got.Status != ActionExpired {
		t.Fatalf("status = %s; want expired", got.Status)
	}
	if _, err := s.ClaimAction(ctx, "act1", actionEpoch.Add(time.Minute)); !errors.Is(err, ErrActionNotPending) {
		t.Fatalf("ClaimAction of an expired action error = %v; want ErrActionNotPending", err)
	}
}

func TestAction_ExpireActionsAndListings(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	newTestAction(t, s, "short", time.Minute)
	newTestAction(t, s, "long", 48*time.Hour)
	now := actionEpoch.Add(time.Hour)

	pending, err := s.ListPendingActions(ctx, now)
	if err != nil || len(pending) != 1 || pending[0].ID != "long" {
		t.Fatalf("ListPendingActions = %+v, %v; want only the unexpired action", pending, err)
	}
	n, err := s.ExpireActions(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("ExpireActions = %d, %v; want 1", n, err)
	}
	decided, err := s.ListDecidedActions(ctx, 10)
	if err != nil || len(decided) != 1 || decided[0].ID != "short" || decided[0].Status != ActionExpired {
		t.Fatalf("ListDecidedActions = %+v, %v; want the expired action", decided, err)
	}
}

func TestAction_UnknownID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.GetAction(ctx, "nope"); !errors.Is(err, ErrActionNotFound) {
		t.Fatalf("GetAction error = %v; want ErrActionNotFound", err)
	}
	if _, err := s.ClaimAction(ctx, "nope", actionEpoch); !errors.Is(err, ErrActionNotFound) {
		t.Fatalf("ClaimAction error = %v; want ErrActionNotFound", err)
	}
	if err := s.RejectAction(ctx, "nope", actionEpoch); !errors.Is(err, ErrActionNotFound) {
		t.Fatalf("RejectAction error = %v; want ErrActionNotFound", err)
	}
}

func TestFinishAction_RejectsNonTerminalStatus(t *testing.T) {
	s := openTestStore(t)
	newTestAction(t, s, "act1", time.Hour)
	if err := s.FinishAction(context.Background(), "act1", ActionRejected, "{}"); err == nil {
		t.Fatal("FinishAction with status rejected: want an error")
	}
}
