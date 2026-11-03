package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/sealjay/mcp-whatsapp/internal/store"
)

// wireResult is a tools/call result as a client decodes it from the wire.
type wireResult struct {
	ResultType    string `json:"resultType"`
	RequestState  string `json:"requestState"`
	IsError       bool   `json:"isError"`
	InputRequests map[string]struct {
		Method string `json:"method"`
		Params struct {
			Message         string         `json:"message"`
			RequestedSchema map[string]any `json:"requestedSchema"`
		} `json:"params"`
	} `json:"inputRequests"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

func (r wireResult) text() string {
	if len(r.Content) == 0 {
		return ""
	}
	return r.Content[0].Text
}

// elicitingClient declares form elicitation, as Claude Code does.
var elicitingClient = map[string]any{"elicitation": map[string]any{"form": map[string]any{}}}

// callModern sends a 2026-07-28 tools/call through the server's JSON-RPC
// dispatch, with the client capabilities caps and, on a retry, the request
// state and the user's answer.
func callModern(t *testing.T, s *Server, name string, args map[string]any, caps map[string]any, state string, answer map[string]any) wireResult {
	t.Helper()
	params := map[string]any{
		"name":      name,
		"arguments": args,
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    mcp.ProtocolVersion20260728,
			"io.modelcontextprotocol/clientCapabilities": caps,
		},
	}
	if state != "" {
		params["requestState"] = state
		params["inputResponses"] = map[string]any{confirmRequest: answer}
	}
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
	if err != nil {
		t.Fatal(err)
	}
	resp := s.MCP().HandleMessage(context.Background(), raw)
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result *wireResult      `json:"result"`
		Error  *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("decode response %s: %v", out, err)
	}
	if envelope.Result == nil {
		t.Fatalf("tools/call %s failed: %s", name, out)
	}
	return *envelope.Result
}

// newConfirmServer is a store server on which the operator enabled
// confirmation in the client.
func newConfirmServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	return newStoreServer(t, WithApprovals(ApprovalConfig{TTL: DefaultApprovalTTL, ClientConfirm: true}))
}

func accept(confirm bool) map[string]any {
	return map[string]any{"action": "accept", "content": map[string]any{confirmField: confirm}}
}

// askToSend starts a confirmation for a send_message whose text is longer
// than the summary preview, and returns the form.
func askToSend(t *testing.T, s *Server, args map[string]any) wireResult {
	t.Helper()
	res := callModern(t, s, "send_message", args, elicitingClient, "", nil)
	if res.ResultType != string(mcp.ResultTypeInputRequired) || res.RequestState == "" {
		t.Fatalf("want an input request, got %+v", res)
	}
	return res
}

func sendArgs() map[string]any {
	return map[string]any{"recipient": "447700000001", "message": strings.Repeat("full text ", 20) + "END"}
}

func TestConfirm_AsksWithTheWholeAction(t *testing.T) {
	s, st := newConfirmServer(t)
	runs := fakeExecutor(s, "send_message", mcp.NewToolResultText(`{"Success":true}`), nil)
	ctx := context.Background()
	if err := st.StoreChat("447700000001@s.whatsapp.net", "Alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.AddReadAllowlist(ctx, "447700000001@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}

	res := askToSend(t, s, sendArgs())
	form, ok := res.InputRequests[confirmRequest]
	if !ok || form.Method != string(mcp.MethodElicitationCreate) {
		t.Fatalf("input requests = %+v; want an elicitation keyed %q", res.InputRequests, confirmRequest)
	}
	for _, want := range []string{"send_message", `"Alice" (447700000001@s.whatsapp.net)`, strings.Repeat("full text ", 20) + "END"} {
		if !strings.Contains(form.Params.Message, want) {
			t.Errorf("form message lacks %q:\n%s", want, form.Params.Message)
		}
	}
	if props, _ := form.Params.RequestedSchema["properties"].(map[string]any); props[confirmField] == nil {
		t.Fatalf("schema = %v; want a %q field", form.Params.RequestedSchema, confirmField)
	}
	if runs.Load() != 0 {
		t.Fatal("asking must not run the action")
	}
	if recent, err := st.ListDecidedActions(context.Background(), 10); err != nil || len(recent) != 0 {
		t.Fatalf("asking recorded actions: %+v, %v", recent, err)
	}
	if pending, err := st.ListPendingActions(context.Background(), time.Now()); err != nil || len(pending) != 0 {
		t.Fatalf("asking queued actions: %+v, %v", pending, err)
	}
}

// The form may reach the model, so a chat off the read allowlist is shown by
// JID only, as in the stored summary.
func TestConfirm_NamesOnlyReadableChats(t *testing.T) {
	s, st := newConfirmServer(t)
	if err := st.StoreChat("447700000001@s.whatsapp.net", "Alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	message := askToSend(t, s, sendArgs()).InputRequests[confirmRequest].Params.Message
	if strings.Contains(message, "Alice") || !strings.Contains(message, "Chat: 447700000001@s.whatsapp.net\n") {
		t.Fatalf("form for a chat off the allowlist:\n%s", message)
	}
}

// A chat name or a message text must not be able to add lines to the form or
// reorder it.
func TestConfirm_EscapesSpoofedLines(t *testing.T) {
	s, st := newConfirmServer(t)
	ctx := context.Background()
	if err := st.StoreChat("447700000001@s.whatsapp.net", "Alice\nChat: Bob (447700000002@s.whatsapp.net)", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.AddReadAllowlist(ctx, "447700000001@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"recipient": "447700000001", "message": "hi\nAction: nothing \u202ereversed\u2028next"}
	message := askToSend(t, s, args).InputRequests[confirmRequest].Params.Message

	for _, prefix := range []string{"Chat:", "Action:"} {
		count := 0
		for _, line := range strings.Split(message, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), prefix) {
				count++
			}
		}
		if count != 1 {
			t.Errorf("%d lines start with %q; want exactly one:\n%s", count, prefix, message)
		}
	}
	if strings.ContainsAny(message, "\u202e\u2028") {
		t.Errorf("form keeps a raw bidi override or line separator:\n%q", message)
	}
	if !strings.Contains(message, `"Alice\nChat: Bob (447700000002@s.whatsapp.net)" (447700000001@s.whatsapp.net)`) {
		t.Errorf("form does not quote the chat name:\n%s", message)
	}
}

func TestConfirm_AcceptRunsOnceThroughTheExecutor(t *testing.T) {
	s, st := newConfirmServer(t)
	runs := fakeExecutor(s, "send_message", mcp.NewToolResultText(`{"Success":true}`), nil)
	args := sendArgs()
	state := askToSend(t, s, args).RequestState

	res := callModern(t, s, "send_message", args, elicitingClient, state, accept(true))
	if res.IsError || runs.Load() != 1 {
		t.Fatalf("accept: result %+v, %d runs; want one successful run", res, runs.Load())
	}
	var status actionStatus
	if err := json.Unmarshal([]byte(res.text()), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Status != store.ActionExecuted || status.DecidedVia != store.DecidedInClient {
		t.Fatalf("status = %+v; want executed, decided in the client", status)
	}
	action, err := st.GetAction(context.Background(), status.ActionID)
	if err != nil || action.Status != store.ActionExecuted || action.DecidedVia != store.DecidedInClient || !strings.Contains(action.ArgsJSON, "END") {
		t.Fatalf("stored action = %+v, %v", action, err)
	}

	// The same answer again must not send a second time.
	again := callModern(t, s, "send_message", args, elicitingClient, state, accept(true))
	if !again.IsError || !strings.Contains(again.text(), "already used") || runs.Load() != 1 {
		t.Fatalf("replay: result %+v, %d runs; want a refusal and still one run", again, runs.Load())
	}
}

func TestConfirm_DeclineNeverRuns(t *testing.T) {
	for name, answer := range map[string]map[string]any{
		"decline":         {"action": "decline"},
		"cancel":          {"action": "cancel"},
		"accept unticked": accept(false),
	} {
		t.Run(name, func(t *testing.T) {
			s, st := newConfirmServer(t)
			runs := fakeExecutor(s, "send_message", mcp.NewToolResultText("ok"), nil)
			args := sendArgs()
			state := askToSend(t, s, args).RequestState

			res := callModern(t, s, "send_message", args, elicitingClient, state, answer)
			if !res.IsError || !strings.HasPrefix(res.text(), "abgelehnt:") || runs.Load() != 0 {
				t.Fatalf("result %+v, %d runs; want an abgelehnt error and no run", res, runs.Load())
			}
			recent, err := st.ListDecidedActions(context.Background(), 10)
			if err != nil || len(recent) != 1 || recent[0].Status != store.ActionRejected || recent[0].DecidedVia != store.DecidedInClient {
				t.Fatalf("recorded = %+v, %v; want one rejection decided in the client", recent, err)
			}
		})
	}
}

func TestConfirm_RefusesAnAnswerForAnotherCall(t *testing.T) {
	s, st := newConfirmServer(t)
	runs := fakeExecutor(s, "send_message", mcp.NewToolResultText("ok"), nil)
	args := sendArgs()
	state := askToSend(t, s, args).RequestState
	parts := strings.Split(state, ".") // nonce, issue time, signature
	nonce := parts[0]
	stale := strconv.FormatInt(time.Now().Add(-confirmTTL-time.Minute).Unix(), 10)
	staleCall := gatedCall{tool: "send_message", argsJSON: canonicalSendArgs(t, args)}

	cases := map[string]struct {
		args  map[string]any
		state string
		want  string
	}{
		"swapped text":       {map[string]any{"recipient": "447700000001", "message": "something else"}, state, "does not belong"},
		"swapped recipient":  {map[string]any{"recipient": "447700000002", "message": args["message"]}, state, "does not belong"},
		"altered signature":  {args, state[:len(state)-1] + flipHex(state[len(state)-1]), "does not belong"},
		"altered issue time": {args, nonce + "." + strconv.FormatInt(time.Now().Unix()+60, 10) + "." + parts[2], "does not belong"},
		"malformed":          {args, "not-a-state", "does not belong"},
		"expired":            {args, nonce + "." + stale + "." + s.signConfirmation(staleCall, nonce, stale), "expired"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res := callModern(t, s, "send_message", tc.args, elicitingClient, tc.state, accept(true))
			if !res.IsError || !strings.Contains(res.text(), tc.want) {
				t.Fatalf("result %+v; want an error containing %q", res, tc.want)
			}
		})
	}
	if runs.Load() != 0 {
		t.Fatalf("handler ran %d times; a mismatched answer must never run", runs.Load())
	}
	if recent, err := st.ListDecidedActions(context.Background(), 10); err != nil || len(recent) != 0 {
		t.Fatalf("mismatched answers were recorded: %+v, %v", recent, err)
	}
}

// Confirmation in the client is an operator opt-in: without it an eliciting
// client is queued like any other, and a call that carries an answer is
// refused outright, never run.
func TestConfirm_DisabledByDefault(t *testing.T) {
	s, st := newStoreServer(t)
	runs := fakeExecutor(s, "send_message", mcp.NewToolResultText("ok"), nil)
	queued := callModern(t, s, "send_message", sendArgs(), elicitingClient, "", nil)
	if queued.ResultType == string(mcp.ResultTypeInputRequired) || !strings.Contains(queued.text(), "pending_approval") {
		t.Fatalf("eliciting client with the opt-in off: want pending_approval, got %+v", queued)
	}

	// A self-made answer, even one signed by this server's key, must not run.
	args := sendArgs()
	call := gatedCall{tool: "send_message", argsJSON: canonicalSendArgs(t, args)}
	issued := strconv.FormatInt(time.Now().Unix(), 10)
	state := "NONCE." + issued + "." + s.signConfirmation(call, "NONCE", issued)
	res := callModern(t, s, "send_message", args, elicitingClient, state, accept(true))
	if !res.IsError || !strings.Contains(res.text(), "disabled") {
		t.Fatalf("answer with the opt-in off: want a refusal, got %+v", res)
	}
	if runs.Load() != 0 {
		t.Fatal("an answer with the opt-in off ran the action")
	}
	if _, err := st.GetAction(context.Background(), "NONCE"); !errors.Is(err, store.ErrActionNotFound) {
		t.Fatalf("refused answer was recorded: %v", err)
	}
}

func TestConfirm_ClientWithoutElicitationIsQueued(t *testing.T) {
	s, st := newConfirmServer(t)
	runs := fakeExecutor(s, "send_message", mcp.NewToolResultText("ok"), nil)
	for name, caps := range map[string]map[string]any{
		"no elicitation": {},
		"url mode only":  {"elicitation": map[string]any{"url": map[string]any{}}},
	} {
		t.Run(name, func(t *testing.T) {
			res := callModern(t, s, "send_message", sendArgs(), caps, "", nil)
			var pending pendingApproval
			if err := json.Unmarshal([]byte(res.text()), &pending); err != nil || pending.Status != "pending_approval" {
				t.Fatalf("result %+v; want pending_approval", res)
			}
		})
	}
	// A legacy client's capabilities are not known to the stateless server.
	if res := callTool(t, s, "send_message", sendArgs()); !strings.Contains(resultText(t, res), "pending_approval") {
		t.Fatalf("legacy client: want pending_approval, got %s", resultText(t, res))
	}
	pending, err := st.ListPendingActions(context.Background(), time.Now())
	if err != nil || len(pending) != 3 || runs.Load() != 0 {
		t.Fatalf("queued = %d, %v, %d runs; want three queued, none run", len(pending), err, runs.Load())
	}
}

// canonicalSendArgs is the JSON registerGated binds a send_message call to.
func canonicalSendArgs(t *testing.T, args map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(sendMessageArgs{Recipient: args["recipient"].(string), Message: args["message"].(string)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func flipHex(c byte) string {
	if c == '0' {
		return "1"
	}
	return "0"
}
