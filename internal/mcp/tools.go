package mcp

// registerTools wires every MCP tool to the bound WhatsApp client.
//
// Tool registrations are split by domain:
//   - tools_query.go   — read-only data retrieval (list_chats, search_contacts, …)
//   - tools_send.go    — outbound messages (send_message, send_file, send_reaction, …)
//   - tools_message.go — message mutation & history (edit, delete, mark_read, sync, …)
//   - tools_groups.go  — group management (create, leave, participants, settings, …)
//   - tools_media.go   — polls + contact cards
//   - tools_privacy.go — blocklist, presence, privacy settings, status message
//   - tools_actions.go — get_action_status for calls queued for operator approval
//
// Read tools are limited to the operator's read allowlist (access.go) and send
// or mutating tools are queued for operator approval (approval.go).
//
// Shared helpers live in tools_core.go (parseTimestamp, normalizeRecipientToChatJID,
// maybeMarkChatRead). Common result helpers live in result.go.
func (s *Server) registerTools() {
	s.registerQueryTools()
	s.registerSendTools()
	s.registerMessageTools()

	// Phase 1 — group management + blocklist.
	s.registerGroupTools()
	s.registerPrivacyTools()

	// Phase 2 — polls + contact cards.
	s.registerMediaTools()

	// Operator approval queue.
	s.registerActionTools()
}
