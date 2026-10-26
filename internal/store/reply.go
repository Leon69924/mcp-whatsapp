package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// maxQuoteSnippetRunes caps the quoted-text snippet kept per reply so a long
// quoted message cannot bloat the cache or the tool output.
const maxQuoteSnippetRunes = 200

// Quote is the reply context captured from a message's ContextInfo at ingest
// time: which message it answers, who wrote it, where it lives and a short
// excerpt of its text. It is what gets stored in the reply_to_* columns.
type Quote struct {
	// MessageID is the quoted message's ID (ContextInfo.StanzaID).
	MessageID string
	// SenderJID is the quoted message's author, LID-resolved.
	SenderJID string
	// ChatJID is the chat holding the quoted message. It differs from the
	// replying message's chat for a private reply to a group message.
	ChatJID string
	// Snippet is the excerpt of the quoted message carried by the reply.
	Snippet string
}

// ReplyInfo is how a reply is shown to MCP clients: the quoted message's
// display-ready sender and text. A reply whose quoted chat is not on the read
// allowlist carries only Restricted, so nothing about that chat leaks.
type ReplyInfo struct {
	MessageID  string `json:"message_id,omitempty"`
	ChatJID    string `json:"chat_jid,omitempty"` // only when it differs from the reply's own chat
	Sender     string `json:"sender,omitempty"`
	Snippet    string `json:"snippet,omitempty"`
	Restricted bool   `json:"restricted,omitempty"` // quoted chat is not on the read allowlist
}

// QuoteSnippet renders the short excerpt of a quoted message the way media
// rows render: "[image] caption" for media (label is the media type), plain
// text otherwise. Whitespace is collapsed and the result truncated.
func QuoteSnippet(label, text string) string {
	text = strings.Join(strings.Fields(text), " ")
	snippet := text
	if label != "" {
		snippet = strings.TrimSpace("[" + label + "] " + text)
	}
	if runes := []rune(snippet); len(runes) > maxQuoteSnippetRunes {
		snippet = string(runes[:maxQuoteSnippetRunes]) + "…"
	}
	return snippet
}

// nullIfEmpty maps "" to SQL NULL so absent reply columns stay NULL.
func nullIfEmpty(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// quoteArgs returns the four reply_to_* column values for a message; all NULL
// when the message is not a reply.
func quoteArgs(q *Quote) (id, sender, chat, snippet sql.NullString) {
	if q == nil {
		return
	}
	return nullIfEmpty(q.MessageID), nullIfEmpty(q.SenderJID), nullIfEmpty(q.ChatJID), nullIfEmpty(q.Snippet)
}

// scanQuote builds the stored Quote from the reply_to_* columns; nil when the
// message is not a reply.
func scanQuote(id, sender, chat, snippet sql.NullString) *Quote {
	if !id.Valid {
		return nil
	}
	return &Quote{MessageID: id.String, SenderJID: sender.String, ChatJID: chat.String, Snippet: snippet.String}
}

// resolveReplies fills ReplyTo on every reply in msgs from its stored Quote.
func (s *Store) resolveReplies(ctx context.Context, allowed AllowedChats, msgs []Message) error {
	for i := range msgs {
		if err := s.resolveReply(ctx, allowed, &msgs[i]); err != nil {
			return err
		}
	}
	return nil
}

// resolveReply renders m.Quote into m.ReplyTo. A quoted chat outside the read
// allowlist yields a bare Restricted marker. Otherwise the quoted message's
// cached row wins over the snippet captured at ingest, because it holds the
// full text and the sender as this store knows them.
func (s *Store) resolveReply(ctx context.Context, allowed AllowedChats, m *Message) error {
	q := m.Quote
	if q == nil {
		return nil
	}
	if !allowed.Has(q.ChatJID) {
		m.ReplyTo = &ReplyInfo{Restricted: true}
		return nil
	}
	info := &ReplyInfo{MessageID: q.MessageID, Snippet: q.Snippet}
	if q.ChatJID != m.ChatJID {
		info.ChatJID = q.ChatJID
	}
	if q.SenderJID != "" {
		info.Sender = s.GetSenderName(ctx, q.SenderJID)
	}

	var (
		content, mediaType, sender sql.NullString
		isFromMe                   bool
	)
	err := s.db.QueryRowContext(ctx,
		"SELECT content, media_type, sender, is_from_me FROM messages WHERE id = ? AND chat_jid = ?",
		q.MessageID, q.ChatJID).Scan(&content, &mediaType, &sender, &isFromMe)
	switch {
	case err == nil:
		if snippet := QuoteSnippet(mediaType.String, content.String); snippet != "" {
			info.Snippet = snippet
		}
		if isFromMe {
			info.Sender = "Me"
		} else if sender.String != "" {
			info.Sender = s.GetSenderName(ctx, sender.String)
		}
	case errors.Is(err, sql.ErrNoRows):
		// The quoted message is not cached; the ingest-time snippet stands.
	default:
		return fmt.Errorf("look up quoted message %s: %w", q.MessageID, err)
	}
	m.ReplyTo = info
	return nil
}
