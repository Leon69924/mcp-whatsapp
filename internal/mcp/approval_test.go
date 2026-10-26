package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/sealjay/mcp-whatsapp/internal/store"
)

// gatedToolNames is every tool that is queued for operator approval.
var gatedToolNames = []string{
	"block_contact", "create_group", "delete_message", "edit_message",
	"join_group_with_link", "leave_group", "send_audio_message",
	"send_contact_card", "send_file", "send_message", "send_poll",
	"send_poll_vote", "send_reaction", "send_reply", "set_group_announce",
	"set_group_locked", "set_group_name", "set_group_topic",
	"set_privacy_setting", "set_status_message", "unblock_contact",
	"update_group_participants",
}

// newStoreServer builds a Server over a real store but no WhatsApp client, so
// a handler that reached the client would panic: a passing test proves the
// code path never touched WhatsApp.
func newStoreServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewServer(nil, nil, withStore(st)), st
}

func callTool(t *testing.T, s *Server, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	tool, ok := s.MCP().ListTools()[name]
	if !ok || tool == nil || tool.Handler == nil {
		t.Fatalf("%s not registered", name)
	}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := tool.Handler(context.Background(), req)
	if err != nil {
		t.Fatalf("%s handler: %v", name, err)
	}
	return res
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("result has no content")
	}
	text, ok := mcp.AsTextContent(res.Content[0])
	if !ok {
		t.Fatalf("content is %T, not text", res.Content[0])
	}
	return text.Text
}

func TestEveryGatedToolHasAnExecutor(t *testing.T) {
	s := NewServer(nil, nil)
	got := make([]string, 0, len(s.executors))
	for name := range s.executors {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(gatedToolNames, ",") {
		t.Fatalf("gated tools = %v; want %v", got, gatedToolNames)
	}
	for _, name := range gatedToolNames {
		desc := s.MCP().GetTool(name).Tool.Description
		if !strings.Contains(desc, "get_action_status") {
			t.Errorf("%s description does not mention operator approval", name)
		}
	}
}

func TestGatedTool_QueuesInsteadOfExecuting(t *testing.T) {
	s, st := newStoreServer(t)
	res := callTool(t, s, "send_message", map[string]any{"recipient": "447700000001", "message": "hello there"})
	if res.IsError {
		t.Fatalf("send_message: %s", resultText(t, res))
	}
	var pending pendingApproval
	if err := json.Unmarshal([]byte(resultText(t, res)), &pending); err != nil {
		t.Fatalf("decode pending result: %v", err)
	}
	if pending.Status != "pending_approval" || pending.ActionID == "" || !strings.HasSuffix(pending.ApproveURL, "/approvals#"+pending.ActionID) {
		t.Fatalf("pending = %+v", pending)
	}
	if !strings.Contains(pending.Summary, `"hello there"`) || !strings.Contains(pending.Summary, "447700000001@s.whatsapp.net") {
		t.Fatalf("summary = %q; want the text preview and the canonical JID", pending.Summary)
	}

	action, err := st.GetAction(context.Background(), pending.ActionID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if action.Status != store.ActionPending || action.Tool != "send_message" || action.TargetJID != "447700000001@s.whatsapp.net" {
		t.Fatalf("stored action = %+v", action)
	}
	if got := action.ExpiresAt.Sub(action.CreatedAt); got != DefaultApprovalTTL {
		t.Fatalf("expiry window = %s; want %s", got, DefaultApprovalTTL)
	}
}

func TestGatedTool_HonoursConfiguredTTL(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := NewServer(nil, nil, withStore(st), WithApprovals(ApprovalConfig{TTL: time.Hour, BaseURL: "http://127.0.0.1:9"}))
	res := callTool(t, s, "set_status_message", map[string]any{"text": "away"})
	var pending pendingApproval
	if err := json.Unmarshal([]byte(resultText(t, res)), &pending); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(pending.ApproveURL, "http://127.0.0.1:9/approvals#") {
		t.Fatalf("approve_url = %q", pending.ApproveURL)
	}
	action, _ := st.GetAction(context.Background(), pending.ActionID)
	if got := action.ExpiresAt.Sub(action.CreatedAt); got != time.Hour {
		t.Fatalf("expiry window = %s; want 1h", got)
	}
}

func TestGatedTool_BadArgumentsFailImmediately(t *testing.T) {
	s, st := newStoreServer(t)
	cases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"missing recipient", "send_message", map[string]any{"message": "x"}},
		{"malformed recipient", "send_message", map[string]any{"recipient": "not a number", "message": "x"}},
		{"too few poll options", "send_poll", map[string]any{"recipient": "447700000001", "question": "q", "options": []string{"a"}}},
		{"missing message id", "delete_message", map[string]any{"chat_jid": "447700000001@s.whatsapp.net"}},
		{"empty participants", "create_group", map[string]any{"name": "g", "participants": []string{}}},
		{"bad participant", "update_group_participants", map[string]any{"chat_jid": "123456789@g.us", "participants": []string{"x y"}, "action": "add"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if res := callTool(t, s, tc.tool, tc.args); !res.IsError {
				t.Fatalf("want an error result, got %s", resultText(t, res))
			}
		})
	}
	pending, err := st.ListPendingActions(context.Background(), time.Now())
	if err != nil || len(pending) != 0 {
		t.Fatalf("rejected calls were queued: %+v, %v", pending, err)
	}
}

func TestGatedTool_MarkChatReadNeedsReadAccess(t *testing.T) {
	s, st := newStoreServer(t)
	args := map[string]any{"recipient": "447700000001", "message": "x", "mark_chat_read": true}
	res := callTool(t, s, "send_message", args)
	if !res.IsError || !strings.Contains(resultText(t, res), "mark_chat_read: "+notAllowlistedReason) {
		t.Fatalf("want a mark_chat_read refusal, got %q", resultText(t, res))
	}
	if err := st.AddReadAllowlist(context.Background(), "447700000001@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}
	if res := callTool(t, s, "send_message", args); res.IsError {
		t.Fatalf("allowlisted chat: %s", resultText(t, res))
	}
}

// The summary goes back to the model, so it may name a chat only when the
// model may read it; a guessed JID must not reveal whose it is.
func TestGatedTool_SummaryNamesOnlyReadableChats(t *testing.T) {
	s, st := newStoreServer(t)
	ctx := context.Background()
	if err := st.StoreChat("447700000001@s.whatsapp.net", "Alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"recipient": "447700000001", "message": "x"}

	var before pendingApproval
	if err := json.Unmarshal([]byte(resultText(t, callTool(t, s, "send_message", args))), &before); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(before.Summary, "Alice") {
		t.Fatalf("summary leaked the name of a non-allowlisted chat: %q", before.Summary)
	}

	if err := st.AddReadAllowlist(ctx, "447700000001@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}
	var after pendingApproval
	if err := json.Unmarshal([]byte(resultText(t, callTool(t, s, "send_message", args))), &after); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after.Summary, "Alice (447700000001@s.whatsapp.net)") {
		t.Fatalf("summary = %q; want the name of an allowlisted chat", after.Summary)
	}
}

// fakeExecutor registers a counting executor under a gated tool name.
func fakeExecutor(s *Server, name string, res *mcp.CallToolResult, err error) *atomic.Int32 {
	var runs atomic.Int32
	s.executors[name] = func(context.Context, json.RawMessage) (*mcp.CallToolResult, error) {
		runs.Add(1)
		return res, err
	}
	return &runs
}

func queueTestAction(t *testing.T, st *store.Store, id string, expires time.Time) {
	t.Helper()
	err := st.CreateAction(context.Background(), store.PendingAction{
		ID: id, Tool: "send_message", ArgsJSON: `{}`, Summary: "s", TargetJID: "1@s.whatsapp.net",
		CreatedAt: time.Now().UTC().Add(-time.Minute), ExpiresAt: expires,
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
}

func TestApproveAction_ExecutesExactlyOnce(t *testing.T) {
	s, st := newStoreServer(t)
	runs := fakeExecutor(s, "send_message", mcp.NewToolResultText(`{"Success":true}`), nil)
	queueTestAction(t, st, "act1", time.Now().Add(time.Hour))

	if err := s.ApproveAction(context.Background(), "act1"); err != nil {
		t.Fatalf("ApproveAction: %v", err)
	}
	if err := s.ApproveAction(context.Background(), "act1"); !errors.Is(err, store.ErrActionNotPending) {
		t.Fatalf("second ApproveAction error = %v; want ErrActionNotPending", err)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("handler ran %d times; want exactly once", got)
	}

	action, _ := st.GetAction(context.Background(), "act1")
	if action.Status != store.ActionExecuted || !strings.Contains(action.ResultJSON, `"is_error":false`) {
		t.Fatalf("action = %+v; want executed with a result", action)
	}

	status := resultText(t, callTool(t, s, "get_action_status", map[string]any{"action_id": "act1"}))
	if !strings.Contains(status, `"status": "executed"`) || !strings.Contains(status, `"result"`) {
		t.Fatalf("get_action_status = %s", status)
	}
}

func TestApproveAction_FailureIsRecorded(t *testing.T) {
	s, st := newStoreServer(t)
	fakeExecutor(s, "send_message", mcp.NewToolResultError("rate limited"), nil)
	queueTestAction(t, st, "act1", time.Now().Add(time.Hour))

	if err := s.ApproveAction(context.Background(), "act1"); err != nil {
		t.Fatalf("ApproveAction: %v", err)
	}
	action, _ := st.GetAction(context.Background(), "act1")
	if action.Status != store.ActionFailed || !strings.Contains(action.ResultJSON, "rate limited") {
		t.Fatalf("action = %+v; want failed with the error text", action)
	}
}

func TestApproveAction_RejectedAndExpiredNeverExecute(t *testing.T) {
	s, st := newStoreServer(t)
	runs := fakeExecutor(s, "send_message", mcp.NewToolResultText("ok"), nil)
	queueTestAction(t, st, "rejected", time.Now().Add(time.Hour))
	queueTestAction(t, st, "expired", time.Now().Add(-time.Second))

	if err := s.RejectAction(context.Background(), "rejected"); err != nil {
		t.Fatalf("RejectAction: %v", err)
	}
	if err := s.ApproveAction(context.Background(), "rejected"); !errors.Is(err, store.ErrActionNotPending) {
		t.Fatalf("approve after reject error = %v; want ErrActionNotPending", err)
	}
	if err := s.ApproveAction(context.Background(), "expired"); !errors.Is(err, store.ErrActionExpired) {
		t.Fatalf("approve after expiry error = %v; want ErrActionExpired", err)
	}
	if got := runs.Load(); got != 0 {
		t.Fatalf("handler ran %d times; rejected and expired actions must never run", got)
	}

	status := resultText(t, callTool(t, s, "get_action_status", map[string]any{"action_id": "expired"}))
	if !strings.Contains(status, `"status": "expired"`) {
		t.Fatalf("get_action_status = %s", status)
	}
	if res := callTool(t, s, "get_action_status", map[string]any{"action_id": "nope"}); !res.IsError {
		t.Fatal("unknown action_id: want an error result")
	}
}

func TestPendingActionsListingExpiresStaleOnes(t *testing.T) {
	s, st := newStoreServer(t)
	queueTestAction(t, st, "live", time.Now().Add(time.Hour))
	queueTestAction(t, st, "stale", time.Now().Add(-time.Second))

	pending, err := s.PendingActions(context.Background())
	if err != nil || len(pending) != 1 || pending[0].ID != "live" {
		t.Fatalf("PendingActions = %+v, %v; want only the live action", pending, err)
	}
	recent, err := s.RecentActions(context.Background(), 10)
	if err != nil || len(recent) != 1 || recent[0].Status != store.ActionExpired {
		t.Fatalf("RecentActions = %+v, %v; want the expired action", recent, err)
	}
}
