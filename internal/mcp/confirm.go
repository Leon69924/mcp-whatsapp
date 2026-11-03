package mcp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/sealjay/mcp-whatsapp/internal/store"
)

// A client that can show the user a form (MCP elicitation) confirms a gated
// call in the client instead of the /approvals queue. The call is answered
// with an input request (SEP-2322 multi round-trip, so the server stays
// stateless): a form that shows the action and asks for confirm=true. The
// client asks the user and retries the call with the answer and the request
// state echoed back.
//
// The request state is a nonce, its issue time and an HMAC over the tool, the
// canonical arguments, the nonce and the issue time under a per-process key.
// An answer therefore counts only for the very call that was asked about, and
// only within confirmTTL; a retry with other arguments is refused. The nonce
// becomes the action's ID in pending_actions, so one answer cannot be used
// twice. An accepted call runs through runAction, the executor an operator
// approval uses, and is recorded executed or failed with decided_via
// "client"; a declined or cancelled one is recorded rejected and never runs.

// confirmTTL is how long the user has to answer a confirmation form.
const confirmTTL = 15 * time.Minute

const (
	confirmRequest = "confirm" // the input request's key
	confirmField   = "confirm" // the boolean the user ticks
)

// declinedResult is what the model gets when the user declines or cancels.
const declinedResult = "abgelehnt: the user declined the confirmation in the client; nothing was sent or changed (action_id %s). Do not call the tool again unless the user asks."

// canElicit reports whether the client of this request can show a
// confirmation form. Only requests of protocol revision 2026-07-28 carry the
// client's capabilities; an older client declared them in initialize, which
// this stateless server does not keep, so such a client gets the queue. An
// elicitation capability that names neither mode is the older shape and
// means form.
func canElicit(ctx context.Context) bool {
	info := server.RequestProtocolInfoFromContext(ctx)
	if info == nil || !info.Modern || info.ClientCapabilities == nil {
		return false
	}
	e := info.ClientCapabilities.Elicitation
	return e != nil && (e.Form != nil || e.URL == nil)
}

// askConfirmation answers call with a confirmation form for the user.
func (s *Server) askConfirmation(call gatedCall) (*mcp.CallToolResult, error) {
	message, err := s.confirmMessage(call)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	nonce := rand.Text()
	issued := strconv.FormatInt(time.Now().Unix(), 10)
	state := nonce + "." + issued + "." + s.signConfirmation(call, nonce, issued)
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{confirmField: map[string]any{
			"type":        "boolean",
			"title":       "Run this action now",
			"description": "Tick and accept to run exactly the action shown; decline to drop it.",
		}},
		"required": []string{confirmField},
	}
	return server.NewInputRequestBuilder(state).
		Elicit(confirmRequest, mcp.ElicitationParams{
			Mode:            mcp.ElicitationModeForm,
			Message:         message,
			RequestedSchema: schema,
		}).
		ToolResult(), nil
}

// confirmMessage words the confirmation form for the user. Unlike the summary
// stored for the model it always names the target chat, as /approvals does:
// it is shown to the person deciding.
func (s *Server) confirmMessage(call gatedCall) (string, error) {
	target := "none (account-level action)"
	if call.target != "" {
		target = call.target
		if name := s.store.DisplayName(call.target); name != "" {
			target = name + " (" + call.target + ")"
		}
	}
	var args bytes.Buffer
	if err := json.Indent(&args, call.argsJSON, "", "  "); err != nil {
		return "", fmt.Errorf("format arguments: %w", err)
	}
	return fmt.Sprintf("WhatsApp: confirm %s\n\nChat: %s\nAction: %s\n\nExactly these arguments will be used:\n%s",
		call.tool, target, call.detail, args.String()), nil
}

// signConfirmation is the HMAC that binds a confirmation to call, nonce and
// issue time.
func (s *Server) signConfirmation(call gatedCall, nonce, issued string) string {
	m := hmac.New(sha256.New, s.confirmKey)
	for _, part := range [][]byte{[]byte(call.tool), call.argsJSON, []byte(nonce), []byte(issued)} {
		m.Write(part)
		m.Write([]byte{0})
	}
	return hex.EncodeToString(m.Sum(nil))
}

var (
	errConfirmMismatch = errors.New("nothing was done: the confirmation does not belong to this call (other arguments, or an altered request state); call the tool again to ask the user anew")
	errConfirmExpired  = errors.New("nothing was done: the confirmation expired before the answer arrived; call the tool again to ask the user anew")
)

// verifyConfirmation checks that state was issued by this process for call
// within confirmTTL of now, and returns its nonce.
func (s *Server) verifyConfirmation(state string, call gatedCall, now time.Time) (string, error) {
	parts := strings.Split(state, ".")
	if len(parts) != 3 {
		return "", errConfirmMismatch
	}
	nonce, issued, mac := parts[0], parts[1], parts[2]
	if !hmac.Equal([]byte(mac), []byte(s.signConfirmation(call, nonce, issued))) {
		return "", errConfirmMismatch
	}
	issuedUnix, err := strconv.ParseInt(issued, 10, 64)
	if err != nil {
		return "", errConfirmMismatch
	}
	if now.After(time.Unix(issuedUnix, 0).Add(confirmTTL)) {
		return "", errConfirmExpired
	}
	return nonce, nil
}

// decideConfirmation handles the retry that carries the user's answer: it
// verifies the answer belongs to call, records the decision and, on an
// accepted confirm=true, runs the action at once.
func (s *Server) decideConfirmation(ctx context.Context, call gatedCall, answer mcp.MultiRoundTripParams) (*mcp.CallToolResult, error) {
	nonce, err := s.verifyConfirmation(answer.RequestState, call, time.Now())
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	response := server.ElicitationResponse(answer.InputResponses, confirmRequest)
	if response == nil {
		return mcp.NewToolResultError("nothing was done: the client sent no answer to the confirmation"), nil
	}
	content, _ := response.Content.(map[string]any)
	accepted := response.Action == mcp.ElicitationResponseActionAccept && content[confirmField] == true

	action, err := s.newAction(ctx, nonce, call)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	status := store.ActionRejected
	if accepted {
		status = store.ActionExecuting
	}
	err = s.store.CreateDecidedAction(ctx, action, status, time.Now().UTC())
	if errors.Is(err, store.ErrActionExists) {
		return mcp.NewToolResultError(fmt.Sprintf("nothing was done: this confirmation was already used (action_id %s); get_action_status reports its outcome", nonce)), nil
	}
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if !accepted {
		return mcp.NewToolResultError(fmt.Sprintf(declinedResult, nonce)), nil
	}

	if err := s.runAction(action); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	done, err := s.store.GetAction(ctx, nonce)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	out, err := s.actionStatusOf(ctx, done)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	res, err := resultJSON(out)
	if err != nil {
		return nil, err
	}
	res.IsError = done.Status == store.ActionFailed
	return res, nil
}
