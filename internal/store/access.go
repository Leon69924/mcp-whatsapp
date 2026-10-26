package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The read allowlist decides which chats the MCP tools may expose. It is
// default-deny: an empty read_allowlist table means the model can read
// nothing. Entries are canonical chat JIDs (a LID-addressed person is stored
// under their phone JID once the LID map knows it); matching expands both
// directions through whatsmeow_lid_map so the @lid and @s.whatsapp.net forms of
// one person are the same chat.

// AllowedEntry is one row of the read allowlist.
type AllowedEntry struct {
	JID     string    `json:"jid"`
	AddedAt time.Time `json:"added_at"`
}

// AccessChat is a known chat together with its read-allowlist state, as shown
// on the operator's access page.
type AccessChat struct {
	JID             string
	Name            string
	IsGroup         bool
	LastMessageTime time.Time
	Allowed         bool
}

// AllowedChats is a point-in-time snapshot of the read allowlist, expanded to
// every JID form under which an allowlisted chat can appear in the cache.
type AllowedChats map[string]struct{}

// Has reports whether jid (device suffix ignored) is an allowlisted chat.
func (a AllowedChats) Has(jid string) bool {
	_, ok := a[stripDevice(jid)]
	return ok
}

// clause returns a SQL condition restricting column to the allowlisted chats.
// An empty allowlist yields a condition that matches nothing.
func (a AllowedChats) clause(column string) (string, []any) {
	if len(a) == 0 {
		return "0 = 1", nil
	}
	args := make([]any, 0, len(a))
	for jid := range a {
		args = append(args, jid)
	}
	sort.Slice(args, func(i, j int) bool { return args[i].(string) < args[j].(string) })
	return column + " IN (?" + strings.Repeat(",?", len(args)-1) + ")", args
}

// stripDevice drops the ":<device>" part of a JID's user segment.
func stripDevice(jid string) string {
	user, server, hasServer := strings.Cut(jid, "@")
	user, _, _ = strings.Cut(user, ":")
	if !hasServer {
		return user
	}
	return user + "@" + server
}

// jidForms returns every JID form of the same chat: the JID itself plus, for a
// person, the other addressing mode the LID map knows. Like ResolveLIDToJID, a
// missing or unreadable LID map means "no other form known", which can only
// narrow what is readable.
func (s *Store) jidForms(jid string) []string {
	jid = stripDevice(jid)
	forms := []string{jid}
	switch {
	case strings.HasSuffix(jid, "@lid"):
		if pn := s.ResolveLIDToJID(jid); pn != jid {
			forms = append(forms, pn)
		}
	case strings.HasSuffix(jid, "@s.whatsapp.net"):
		forms = append(forms, s.lidsForPhone(jidUser(jid))...)
	}
	return forms
}

// lidsForPhone returns the LID JIDs the LID map holds for a phone number.
func (s *Store) lidsForPhone(phone string) []string {
	if s.whatsmeowDB == nil {
		return nil
	}
	rows, err := s.whatsmeowDB.Query("SELECT lid FROM whatsmeow_lid_map WHERE pn = ?", phone)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var lids []string
	for rows.Next() {
		var lid string
		if err := rows.Scan(&lid); err != nil {
			return nil
		}
		lids = append(lids, lid+"@lid")
	}
	return lids
}

// canonicalAllowlistJID is the form an allowlist entry is stored under.
func (s *Store) canonicalAllowlistJID(jid string) (string, error) {
	jid = strings.TrimSpace(jid)
	if !strings.Contains(jid, "@") {
		return "", fmt.Errorf("chat_jid: must be a WhatsApp JID, got %q", jid)
	}
	return s.ResolveLIDToJID(stripDevice(jid)), nil
}

// ListReadAllowlist returns the allowlist entries, oldest first.
func (s *Store) ListReadAllowlist(ctx context.Context) ([]AllowedEntry, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT chat_jid, added_at FROM read_allowlist ORDER BY added_at, chat_jid")
	if err != nil {
		return nil, fmt.Errorf("list read allowlist: %w", err)
	}
	defer rows.Close()
	var entries []AllowedEntry
	for rows.Next() {
		var e AllowedEntry
		var addedAt sql.NullTime
		if err := rows.Scan(&e.JID, &addedAt); err != nil {
			return nil, fmt.Errorf("scan read allowlist: %w", err)
		}
		e.AddedAt = addedAt.Time
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// AddReadAllowlist makes a chat readable. Adding a chat that is already
// allowlisted (under either of its JID forms) is a no-op.
func (s *Store) AddReadAllowlist(ctx context.Context, jid string) error {
	canonical, err := s.canonicalAllowlistJID(jid)
	if err != nil {
		return err
	}
	allowed, err := s.AllowedChats(ctx)
	if err != nil {
		return err
	}
	if allowed.Has(canonical) {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO read_allowlist (chat_jid, added_at) VALUES (?, ?)",
		canonical, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("add to read allowlist: %w", err)
	}
	return nil
}

// RemoveReadAllowlist makes a chat unreadable again, whichever JID form it was
// allowlisted under. Removing a chat that is not allowlisted is a no-op.
func (s *Store) RemoveReadAllowlist(ctx context.Context, jid string) error {
	if !strings.Contains(jid, "@") {
		return fmt.Errorf("chat_jid: must be a WhatsApp JID, got %q", jid)
	}
	forms := s.jidForms(jid)
	args := make([]any, len(forms))
	for i, f := range forms {
		args[i] = f
	}
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM read_allowlist WHERE chat_jid IN (?"+strings.Repeat(",?", len(forms)-1)+")",
		args...,
	); err != nil {
		return fmt.Errorf("remove from read allowlist: %w", err)
	}
	return nil
}

// AllowedChats snapshots the read allowlist, expanded to every JID form.
func (s *Store) AllowedChats(ctx context.Context) (AllowedChats, error) {
	entries, err := s.ListReadAllowlist(ctx)
	if err != nil {
		return nil, err
	}
	allowed := make(AllowedChats, len(entries))
	for _, e := range entries {
		for _, form := range s.jidForms(e.JID) {
			allowed[form] = struct{}{}
		}
	}
	return allowed, nil
}

// IsChatAllowed reports whether the chat is on the read allowlist.
func (s *Store) IsChatAllowed(ctx context.Context, jid string) (bool, error) {
	allowed, err := s.AllowedChats(ctx)
	if err != nil {
		return false, err
	}
	return allowed.Has(jid), nil
}

// DisplayName returns the best known name for a chat: the synced address-book
// name, else the cached chat name, else "". It does not consult the allowlist;
// callers decide whether the name may be shown.
func (s *Store) DisplayName(jid string) string {
	if name := s.ContactName(jid); name != "" {
		return name
	}
	return s.FindChatName(jid)
}

// ReplaceReadAllowlist makes jids the whole read allowlist in one transaction:
// every other entry is removed, so an empty slice denies everything. Each JID
// is stored in its canonical form, and chats that stay allowlisted keep their
// original added_at. Either the whole replacement is applied or none of it.
func (s *Store) ReplaceReadAllowlist(ctx context.Context, jids []string) error {
	canonical := make([]string, 0, len(jids))
	seen := make(map[string]struct{}, len(jids))
	for _, jid := range jids {
		c, err := s.canonicalAllowlistJID(jid)
		if err != nil {
			return err
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		canonical = append(canonical, c)
	}

	entries, err := s.ListReadAllowlist(ctx)
	if err != nil {
		return err
	}
	addedAt := make(map[string]time.Time, len(entries))
	for _, e := range entries {
		addedAt[s.ResolveLIDToJID(e.JID)] = e.AddedAt
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace read allowlist: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM read_allowlist"); err != nil {
		return fmt.Errorf("replace read allowlist: clear: %w", err)
	}
	now := time.Now().UTC()
	for _, jid := range canonical {
		at, ok := addedAt[jid]
		if !ok || at.IsZero() {
			at = now
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO read_allowlist (chat_jid, added_at) VALUES (?, ?)", jid, at,
		); err != nil {
			return fmt.Errorf("replace read allowlist: insert %s: %w", jid, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace read allowlist: commit: %w", err)
	}
	return nil
}

// ListAccessChats returns every chat the operator can allow, one row per
// canonical JID, with its allowlist state: the cached chats, the extra chats the
// caller knows about (the groups the account is in, which may have no cached
// message) and allowlist entries without any chat, so the operator can still
// remove them. The @lid and phone forms of one person collapse into one row
// under the phone JID. Allowed chats come first, then the most recently active;
// chats that never had a message come last.
func (s *Store) ListAccessChats(ctx context.Context, extra []AccessChat) ([]AccessChat, error) {
	allowed, err := s.AllowedChats(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := s.ListReadAllowlist(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, "SELECT jid, name, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, fmt.Errorf("list access chats: %w", err)
	}
	var candidates []AccessChat
	for rows.Next() {
		var (
			c        AccessChat
			name     sql.NullString
			lastTime sql.NullTime
		)
		if err := rows.Scan(&c.JID, &name, &lastTime); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan access chat: %w", err)
		}
		c.Name = name.String
		c.LastMessageTime = lastTime.Time
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	candidates = append(candidates, extra...)
	for _, e := range entries {
		candidates = append(candidates, AccessChat{JID: e.JID, Name: s.DisplayName(e.JID)})
	}

	byJID := make(map[string]int, len(candidates))
	chats := make([]AccessChat, 0, len(candidates))
	for _, c := range candidates {
		c.IsGroup = strings.HasSuffix(c.JID, "@g.us")
		if name := s.ContactName(c.JID); name != "" && !c.IsGroup {
			c.Name = name
		}
		c.JID = s.ResolveLIDToJID(stripDevice(c.JID))
		c.Allowed = allowed.Has(c.JID)
		i, ok := byJID[c.JID]
		if !ok {
			byJID[c.JID] = len(chats)
			chats = append(chats, c)
			continue
		}
		merged := &chats[i]
		if merged.Name == "" {
			merged.Name = c.Name
		}
		if c.LastMessageTime.After(merged.LastMessageTime) {
			merged.LastMessageTime = c.LastMessageTime
		}
	}
	sort.SliceStable(chats, func(i, j int) bool {
		if chats[i].Allowed != chats[j].Allowed {
			return chats[i].Allowed
		}
		return chats[i].LastMessageTime.After(chats[j].LastMessageTime)
	})
	return chats, nil
}
