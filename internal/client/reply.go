package client

import (
	waProto "go.mau.fi/whatsmeow/binary/proto"

	"github.com/sealjay/mcp-whatsapp/internal/store"
)

// contextInfoCarrier is implemented by every whatsmeow message type that can
// quote another message. The generated getters are nil-safe, so a typed nil
// pointer is a valid (empty) carrier.
type contextInfoCarrier interface {
	GetContextInfo() *waProto.ContextInfo
}

// contextInfoOf returns the ContextInfo of the first message type in msg that
// carries one: the types the ingest path stores plus the other user-visible
// types a reply can come from.
func contextInfoOf(msg *waProto.Message) *waProto.ContextInfo {
	carriers := []contextInfoCarrier{
		msg.GetExtendedTextMessage(),
		msg.GetImageMessage(),
		msg.GetVideoMessage(),
		msg.GetAudioMessage(),
		msg.GetDocumentMessage(),
		msg.GetStickerMessage(),
		msg.GetLocationMessage(),
		msg.GetLiveLocationMessage(),
		msg.GetContactMessage(),
		msg.GetContactsArrayMessage(),
		msg.GetPollCreationMessage(),
		msg.GetEventMessage(),
	}
	for _, carrier := range carriers {
		if ci := carrier.GetContextInfo(); ci != nil {
			return ci
		}
	}
	return nil
}

// extractQuote returns the reply context of msg, or nil when msg does not
// quote another message. ContextInfo that carries only forwarding, ephemeral
// or mention data has no StanzaID and is not a reply. chatJID is the
// (LID-normalised) chat msg belongs to; it is the quoted chat unless the
// reply names another one (a private reply to a group message).
func (c *Client) extractQuote(msg *waProto.Message, chatJID string) *store.Quote {
	ci := contextInfoOf(msg)
	if ci == nil || ci.GetStanzaID() == "" {
		return nil
	}
	quotedChat := chatJID
	if remote := ci.GetRemoteJID(); remote != "" {
		quotedChat = c.store.ResolveLIDToJID(remote)
	}
	return &store.Quote{
		MessageID: ci.GetStanzaID(),
		SenderJID: c.store.ResolveLIDToJID(ci.GetParticipant()),
		ChatJID:   quotedChat,
		Snippet:   quotedSnippet(ci.GetQuotedMessage()),
	}
}

// quotedSnippet renders the quoted message the way media rows render:
// "[image] caption", "[poll] question", or the plain text.
func quotedSnippet(q *waProto.Message) string {
	if q == nil {
		return ""
	}
	if text := extractTextContent(q); text != "" {
		return store.QuoteSnippet("", text)
	}
	if mediaType, _, _, _, _, _, _, _ := extractMediaInfo(q); mediaType != "" {
		return store.QuoteSnippet(mediaType, mediaCaption(q))
	}
	switch {
	case q.GetPollCreationMessage() != nil:
		return store.QuoteSnippet("poll", q.GetPollCreationMessage().GetName())
	case q.GetStickerMessage() != nil:
		return store.QuoteSnippet("sticker", "")
	case q.GetLocationMessage() != nil, q.GetLiveLocationMessage() != nil:
		return store.QuoteSnippet("location", "")
	case q.GetContactMessage() != nil:
		return store.QuoteSnippet("contact", q.GetContactMessage().GetDisplayName())
	case q.GetContactsArrayMessage() != nil:
		return store.QuoteSnippet("contact", q.GetContactsArrayMessage().GetDisplayName())
	}
	return ""
}

// mediaCaption returns the caption of an image, video or document message.
func mediaCaption(msg *waProto.Message) string {
	switch {
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage().GetCaption()
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage().GetCaption()
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage().GetCaption()
	}
	return ""
}
