package mcp

import (
	"context"
	"strings"
	"testing"
	"time"
)

// With an empty allowlist every tool that names a chat refuses it, and does so
// before reaching WhatsApp (the client is nil here).
func TestReadTools_RefuseChatsOffTheAllowlist(t *testing.T) {
	s, _ := newStoreServer(t)
	const chat = "447700000001@s.whatsapp.net"
	cases := []struct {
		tool string
		args map[string]any
	}{
		{"list_messages", map[string]any{"chat_jid": chat}},
		{"get_chat", map[string]any{"chat_jid": chat}},
		{"download_media", map[string]any{"chat_jid": chat, "message_id": "m1"}},
		{"get_poll_results", map[string]any{"chat_jid": chat, "poll_message_id": "p1"}},
		{"get_group_info", map[string]any{"chat_jid": "123456789@g.us"}},
		{"get_group_invite_link", map[string]any{"chat_jid": "123456789@g.us"}},
		{"send_typing", map[string]any{"chat_jid": chat, "active": true}},
		{"mark_read", map[string]any{"chat_jid": chat, "message_ids": []string{"m1"}}},
		{"mark_chat_read", map[string]any{"chat_jid": chat}},
		{"request_sync", map[string]any{"chat_jid": chat}},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			res := callTool(t, s, tc.tool, tc.args)
			if !res.IsError || !strings.Contains(resultText(t, res), "chat_jid: "+notAllowlistedReason) {
				t.Fatalf("want an allowlist refusal, got error=%v %q", res.IsError, resultText(t, res))
			}
		})
	}
}

func TestReadTools_BareNumberAndDeviceFormsAreChecked(t *testing.T) {
	s, _ := newStoreServer(t)
	for _, jid := range []string{"447700000001", "447700000001:5@s.whatsapp.net"} {
		res := callTool(t, s, "get_chat", map[string]any{"chat_jid": jid})
		if !res.IsError || !strings.Contains(resultText(t, res), notAllowlistedReason) {
			t.Fatalf("get_chat(%s) = %q; want an allowlist refusal", jid, resultText(t, res))
		}
	}
}

func TestReadTools_ListingsAreEmptyByDefaultAndOpenAfterAllowlisting(t *testing.T) {
	s, st := newStoreServer(t)
	ctx := context.Background()
	if err := st.StoreChat("447700000001@s.whatsapp.net", "Alice", time.Now()); err != nil {
		t.Fatal(err)
	}

	if got := resultText(t, callTool(t, s, "list_chats", map[string]any{})); strings.Contains(got, "Alice") {
		t.Fatalf("list_chats leaked a non-allowlisted chat: %s", got)
	}
	if got := resultText(t, callTool(t, s, "search_contacts", map[string]any{"query": "Alice"})); strings.Contains(got, "Alice") {
		t.Fatalf("search_contacts leaked a non-allowlisted contact: %s", got)
	}

	if err := st.AddReadAllowlist(ctx, "447700000001@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}
	if got := resultText(t, callTool(t, s, "list_chats", map[string]any{})); !strings.Contains(got, "Alice") {
		t.Fatalf("list_chats after allowlisting = %s; want Alice", got)
	}
	if got := resultText(t, callTool(t, s, "search_contacts", map[string]any{"query": "Alice"})); !strings.Contains(got, "Alice") {
		t.Fatalf("search_contacts after allowlisting = %s; want Alice", got)
	}
}
