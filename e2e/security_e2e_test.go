//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"
)

func TestSendFileRejectsOutOfRootPath(t *testing.T) {
	h := newHarness(t)
	h.initializeMCP()

	res := h.callTool("send_file", map[string]any{
		"recipient":  "00000000@s.whatsapp.net",
		"media_path": "/etc/passwd",
	})
	if !res.IsError {
		t.Fatalf("expected error result, got success: %+v", res)
	}
	if !strings.Contains(res.Text, "allowed root") {
		t.Fatalf("error should mention allowed root, got %q", res.Text)
	}
}

func TestDownloadMediaRejectsOutOfRootOutputPath(t *testing.T) {
	h := newHarness(t, "00000000@s.whatsapp.net")
	h.initializeMCP()

	res := h.callTool("download_media", map[string]any{
		"message_id":  "any",
		"chat_jid":    "00000000@s.whatsapp.net",
		"output_path": "/etc/passwd",
	})
	// download_media returns a JSON {Success, Message, ...} payload rather
	// than an MCP tool error, so check the body for the rejection string.
	if !strings.Contains(res.Text, "allowed root") {
		t.Fatalf("error should mention allowed root, got %q", res.Text)
	}
}

func TestDownloadMediaRefusesChatOffTheAllowlist(t *testing.T) {
	h := newHarness(t)
	h.initializeMCP()

	res := h.callTool("download_media", map[string]any{
		"message_id": "any",
		"chat_jid":   "00000000@s.whatsapp.net",
	})
	if !res.IsError || !strings.Contains(res.Text, "read allowlist") {
		t.Fatalf("expected a read-allowlist refusal, got %+v", res)
	}
}

// TestSendMessageIsQueuedForApproval: a legacy client, whose capabilities the
// stateless server does not keep, cannot confirm in the client, so the call is
// queued for /approvals.
func TestSendMessageIsQueuedForApproval(t *testing.T) {
	h := newHarness(t)
	h.initializeMCP()

	res := h.callTool("send_message", map[string]any{
		"recipient": "00000000",
		"message":   "hello",
	})
	if res.IsError || !strings.Contains(res.Text, "pending_approval") {
		t.Fatalf("expected a pending_approval result, got %+v", res)
	}
}

// TestSendMessageAsksAModernClientToConfirm: with the operator's opt-in and a
// token, a 2026-07-28 client declaring form elicitation gets a confirmation
// form instead of the queue; declining it does nothing, and an answer to
// another call is refused. Without the token /mcp answers 401.
func TestSendMessageAsksAModernClientToConfirm(t *testing.T) {
	h := newConfirmHarness(t)
	caps := map[string]any{"elicitation": map[string]any{"form": map[string]any{}}}
	args := map[string]any{"recipient": "00000000", "message": "hello"}

	for _, token := range []string{"", "wrong"} {
		resp, raw := h.postModern("send_message", args, caps, nil, token)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: status %d (%s); want 401", token, resp.StatusCode, raw)
		}
	}

	ask := h.callToolModern("send_message", args, caps, nil)
	form, ok := ask.InputRequests["confirm"]
	if ask.ResultType != "input_required" || ask.RequestState == "" || !ok || form.Method != "elicitation/create" ||
		!strings.Contains(form.Params.Message, "00000000@s.whatsapp.net") || !strings.Contains(form.Params.Message, "hello") {
		t.Fatalf("expected a confirmation form for the call, got %+v", ask)
	}

	swapped := h.callToolModern("send_message", map[string]any{"recipient": "00000000", "message": "other"}, caps, map[string]any{
		"requestState":   ask.RequestState,
		"inputResponses": map[string]any{"confirm": map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}},
	})
	if !swapped.IsError || !strings.Contains(swapped.text(), "does not belong") {
		t.Fatalf("expected an answer for other arguments to be refused, got %+v", swapped)
	}

	declined := h.callToolModern("send_message", args, caps, map[string]any{
		"requestState":   ask.RequestState,
		"inputResponses": map[string]any{"confirm": map[string]any{"action": "decline"}},
	})
	if !declined.IsError || !strings.HasPrefix(declined.text(), "abgelehnt:") {
		t.Fatalf("expected an abgelehnt result, got %+v", declined)
	}

	// Without elicitation the same client is queued, as before.
	queued := h.callToolModern("send_message", args, map[string]any{}, nil)
	if queued.IsError || !strings.Contains(queued.text(), "pending_approval") {
		t.Fatalf("expected pending_approval without elicitation, got %+v", queued)
	}
}

// TestClientConfirmIsOffByDefault: without WHATSAPP_MCP_CLIENT_CONFIRM an
// eliciting client is queued, and a call carrying an answer is refused.
func TestClientConfirmIsOffByDefault(t *testing.T) {
	h := newHarness(t)
	caps := map[string]any{"elicitation": map[string]any{"form": map[string]any{}}}
	args := map[string]any{"recipient": "00000000", "message": "hello"}

	queued := h.callToolModern("send_message", args, caps, nil)
	if queued.IsError || !strings.Contains(queued.text(), "pending_approval") {
		t.Fatalf("expected pending_approval with the opt-in off, got %+v", queued)
	}
	forged := h.callToolModern("send_message", args, caps, map[string]any{
		"requestState":   "x.1.y",
		"inputResponses": map[string]any{"confirm": map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}},
	})
	if !forged.IsError || !strings.Contains(forged.text(), "disabled") {
		t.Fatalf("expected an answer to be refused with the opt-in off, got %+v", forged)
	}
}
