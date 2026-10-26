package client

import (
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"

	"github.com/sealjay/mcp-whatsapp/internal/store"
)

func normalizeForTest(t *testing.T, c *Client, chat string, raw *waProto.Message) store.Message {
	t.Helper()
	return c.normalizeIncomingMessage(
		chat, "447700000001@s.whatsapp.net", "447700000001",
		false, false, "", "reply-1",
		time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), raw,
	).msg
}

func TestExtractQuote_ExtendedTextReply(t *testing.T) {
	s := newTestStoreWithLIDMap(t, map[string]string{"99887766": "447700000002"})
	c := newClientWithStore(t, s)

	msg := normalizeForTest(t, c, "447700000001@s.whatsapp.net", &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text: proto.String("sure"),
			ContextInfo: &waProto.ContextInfo{
				StanzaID:      proto.String("orig-1"),
				Participant:   proto.String("99887766@lid"),
				QuotedMessage: &waProto.Message{Conversation: proto.String("can you  join\nat 5?")},
			},
		},
	})

	want := &store.Quote{
		MessageID: "orig-1",
		SenderJID: "447700000002@s.whatsapp.net",
		ChatJID:   "447700000001@s.whatsapp.net",
		Snippet:   "can you join at 5?",
	}
	if msg.Quote == nil || *msg.Quote != *want {
		t.Fatalf("Quote = %+v, want %+v", msg.Quote, want)
	}
}

func TestExtractQuote_ImageCaptionReply(t *testing.T) {
	s := newTestStoreWithLIDMap(t, nil)
	c := newClientWithStore(t, s)

	msg := normalizeForTest(t, c, "447700000001@s.whatsapp.net", &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			ContextInfo: &waProto.ContextInfo{
				StanzaID: proto.String("orig-2"),
				QuotedMessage: &waProto.Message{
					ImageMessage: &waProto.ImageMessage{Caption: proto.String("sunset")},
				},
			},
		},
	})

	if msg.Quote == nil || msg.Quote.Snippet != "[image] sunset" {
		t.Fatalf("Quote = %+v, want snippet %q", msg.Quote, "[image] sunset")
	}
}

func TestExtractQuote_PrivateReplyFromGroupUsesRemoteJID(t *testing.T) {
	s := newTestStoreWithLIDMap(t, nil)
	c := newClientWithStore(t, s)

	msg := normalizeForTest(t, c, "447700000001@s.whatsapp.net", &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text: proto.String("replying privately"),
			ContextInfo: &waProto.ContextInfo{
				StanzaID:    proto.String("grp-9"),
				Participant: proto.String("447700000002@s.whatsapp.net"),
				RemoteJID:   proto.String("123456789@g.us"),
			},
		},
	})

	if msg.Quote == nil || msg.Quote.ChatJID != "123456789@g.us" {
		t.Fatalf("Quote = %+v, want ChatJID 123456789@g.us", msg.Quote)
	}
}

func TestExtractQuote_ContextInfoWithoutStanzaIDIsNotAReply(t *testing.T) {
	s := newTestStoreWithLIDMap(t, nil)
	c := newClientWithStore(t, s)

	msg := normalizeForTest(t, c, "447700000001@s.whatsapp.net", &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text: proto.String("forwarded"),
			ContextInfo: &waProto.ContextInfo{
				IsForwarded:  proto.Bool(true),
				MentionedJID: []string{"447700000002@s.whatsapp.net"},
			},
		},
	})

	if msg.Quote != nil {
		t.Fatalf("Quote = %+v, want nil for forward/mention-only ContextInfo", msg.Quote)
	}
}
