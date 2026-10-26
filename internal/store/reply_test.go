package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func storeReply(t *testing.T, s *Store, id, chat string, q *Quote) {
	t.Helper()
	err := s.StoreMessage(context.Background(), Message{
		ID:        id,
		ChatJID:   chat,
		Sender:    "447700000002",
		Content:   "my answer",
		Timestamp: time.Date(2026, 1, 11, 9, 0, 0, 0, time.UTC),
		Quote:     q,
	}, nil, nil, nil, 0)
	if err != nil {
		t.Fatalf("StoreMessage: %v", err)
	}
}

func listAlice(t *testing.T, s *Store) []Message {
	t.Helper()
	msgs, err := s.ListMessages(context.Background(), ListMessagesParams{
		ChatJID: "447700000001@s.whatsapp.net", Limit: 50,
	})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	return msgs
}

func findMsg(t *testing.T, msgs []Message, id string) Message {
	t.Helper()
	for _, m := range msgs {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("message %s not returned", id)
	return Message{}
}

func TestReply_RoundTripPrefersCachedQuotedMessage(t *testing.T) {
	s := openTestStore(t)
	// a1 ("hello alice", sent by Alice) is cached; the stored snippet is stale.
	storeReply(t, s, "r1", "447700000001@s.whatsapp.net", &Quote{
		MessageID: "a1",
		SenderJID: "447700000001@s.whatsapp.net",
		ChatJID:   "447700000001@s.whatsapp.net",
		Snippet:   "stale",
	})

	got := findMsg(t, listAlice(t, s), "r1")
	want := ReplyInfo{MessageID: "a1", Sender: "Alice", Snippet: "hello alice"}
	if got.ReplyTo == nil || *got.ReplyTo != want {
		t.Fatalf("ReplyTo = %+v, want %+v", got.ReplyTo, want)
	}
	if plain := findMsg(t, listAlice(t, s), "a1"); plain.ReplyTo != nil {
		t.Fatalf("non-reply has ReplyTo %+v", plain.ReplyTo)
	}
}

func TestReply_UncachedQuoteFallsBackToStoredSnippet(t *testing.T) {
	s := openTestStore(t)
	storeReply(t, s, "r2", "447700000001@s.whatsapp.net", &Quote{
		MessageID: "gone-1",
		SenderJID: "447700000002@s.whatsapp.net",
		ChatJID:   "447700000001@s.whatsapp.net",
		Snippet:   "[image] sunset",
	})

	got := findMsg(t, listAlice(t, s), "r2")
	want := ReplyInfo{MessageID: "gone-1", Sender: "Bob", Snippet: "[image] sunset"}
	if got.ReplyTo == nil || *got.ReplyTo != want {
		t.Fatalf("ReplyTo = %+v, want %+v", got.ReplyTo, want)
	}
}

func TestReply_PrivateReplyFromGroupNamesQuotedChat(t *testing.T) {
	s := openTestStore(t)
	storeReply(t, s, "r3", "447700000001@s.whatsapp.net", &Quote{
		MessageID: "g1",
		SenderJID: "447700000001@s.whatsapp.net",
		ChatJID:   "123456789@g.us",
	})

	mc, err := s.GetMessageContext(context.Background(), "r3", 1, 1)
	if err != nil {
		t.Fatalf("GetMessageContext: %v", err)
	}
	want := ReplyInfo{MessageID: "g1", ChatJID: "123456789@g.us", Sender: "Alice", Snippet: "team kickoff"}
	if mc.Message.ReplyTo == nil || *mc.Message.ReplyTo != want {
		t.Fatalf("ReplyTo = %+v, want %+v", mc.Message.ReplyTo, want)
	}
}

func TestReply_QuotedChatOutsideAllowlistIsRedacted(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	storeReply(t, s, "r4", "447700000001@s.whatsapp.net", &Quote{
		MessageID: "g1",
		SenderJID: "447700000001@s.whatsapp.net",
		ChatJID:   "123456789@g.us",
		Snippet:   "team kickoff",
	})
	if err := s.RemoveReadAllowlist(ctx, "123456789@g.us"); err != nil {
		t.Fatalf("RemoveReadAllowlist: %v", err)
	}

	got := findMsg(t, listAlice(t, s), "r4")
	if got.ReplyTo == nil || *got.ReplyTo != (ReplyInfo{Restricted: true}) {
		t.Fatalf("ReplyTo = %+v, want only Restricted", got.ReplyTo)
	}
	out := s.FormatMessage(ctx, got, false)
	for _, leak := range []string{"g1", "team kickoff", "Alice (", "123456789"} {
		if strings.Contains(out, leak) {
			t.Fatalf("redacted reply leaks %q: %q", leak, out)
		}
	}
	if !strings.Contains(out, "reply_to: a message in a chat that is not on the read allowlist") {
		t.Fatalf("missing restricted marker: %q", out)
	}
}

func TestFormatMessage_ReplyLine(t *testing.T) {
	s := openTestStore(t)
	m := Message{
		ID: "r5", ChatJID: "447700000001@s.whatsapp.net", Sender: "me", Content: "ok", IsFromMe: true,
		Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		ReplyTo:   &ReplyInfo{MessageID: "a1", Sender: "Alice", Snippet: "hello alice"},
	}
	want := "[2026-01-02 03:04:05] From: Me: ok\n  reply_to: Alice (message ID: a1): hello alice\n"
	if got := s.FormatMessage(context.Background(), m, false); got != want {
		t.Fatalf("FormatMessage = %q, want %q", got, want)
	}
}

func TestQuoteSnippet(t *testing.T) {
	if got := QuoteSnippet("image", "  a\n b "); got != "[image] a b" {
		t.Fatalf("QuoteSnippet media = %q", got)
	}
	long := strings.Repeat("x", maxQuoteSnippetRunes+10)
	if got := QuoteSnippet("", long); len([]rune(got)) != maxQuoteSnippetRunes+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("QuoteSnippet did not truncate: %d runes", len([]rune(got)))
	}
}
