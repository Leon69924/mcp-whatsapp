package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"go.mau.fi/whatsmeow/types"

	"github.com/sealjay/mcp-whatsapp/internal/client"
)

// The operator's read allowlist decides which chats the tools may expose; the
// default is that none are readable. The store layer filters every query it
// answers, so this file covers what the store cannot: refusing a specific
// non-allowlisted chat with an explanation, and tools that read from the live
// WhatsApp API instead of SQLite.

// notAllowlistedReason is the tail of every refusal; the operator manages the
// allowlist at /access.
const notAllowlistedReason = "not on the read allowlist; the operator manages it at /access"

// allowlistNote is appended to the description of every tool whose output is
// limited to allowlisted chats, so the model knows why results may be missing.
const allowlistNote = " Results are limited to chats on the operator's read allowlist (managed at /access); everything else is omitted."

// readDesc adds the allowlist note to a read tool's description.
func readDesc(description string) string { return description + allowlistNote }

// chatJIDArg parses a chat or recipient argument — a bare phone number or a
// JID — into the canonical JID string (no device suffix) the allowlist and the
// cache key chats by. Errors follow the `<field>: <reason>` shape.
func chatJIDArg(field, raw string) (string, error) {
	canonical, err := client.NormalizeJIDArg(field, raw)
	if err != nil {
		return "", err
	}
	if !strings.Contains(canonical, "@") {
		canonical += "@" + types.DefaultUserServer
	}
	jid, err := types.ParseJID(canonical)
	if err != nil {
		return "", fmt.Errorf("%s: invalid JID: %w", field, err)
	}
	return jid.ToNonAD().String(), nil
}

// checkReadable returns an error unless the chat named by raw is on the read
// allowlist.
func (s *Server) checkReadable(ctx context.Context, field, raw string) error {
	jid, err := chatJIDArg(field, raw)
	if err != nil {
		return err
	}
	return s.checkJIDReadable(ctx, field, jid)
}

// checkJIDReadable is checkReadable for an already-canonical JID.
func (s *Server) checkJIDReadable(ctx context.Context, field, jid string) error {
	allowed, err := s.store.IsChatAllowed(ctx, jid)
	if err != nil {
		return fmt.Errorf("%s: read allowlist lookup failed: %w", field, err)
	}
	if !allowed {
		return fmt.Errorf("%s: %s", field, notAllowlistedReason)
	}
	return nil
}

// requireReadable is checkReadable as a tool-error result, nil when readable.
func (s *Server) requireReadable(ctx context.Context, field, raw string) *mcp.CallToolResult {
	if err := s.checkReadable(ctx, field, raw); err != nil {
		return mcp.NewToolResultError(err.Error())
	}
	return nil
}

// requireMarkChatReadAllowed refuses a send's mark_chat_read side effect on a
// chat the model may not read: acking a chat is a per-chat action.
func (s *Server) requireMarkChatReadAllowed(ctx context.Context, requested bool, chatJID string) *mcp.CallToolResult {
	if !requested {
		return nil
	}
	if err := s.checkJIDReadable(ctx, "mark_chat_read", chatJID); err != nil {
		return mcp.NewToolResultError(err.Error())
	}
	return nil
}
