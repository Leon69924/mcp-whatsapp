package store

import (
	"context"
	"testing"
)

// clearAllowlist empties the allowlist the seed fixture grants to every
// seeded chat, so a test starts from the production default: nothing readable.
func clearAllowlist(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec("DELETE FROM read_allowlist"); err != nil {
		t.Fatalf("clear allowlist: %v", err)
	}
}

func TestReadAllowlist_DefaultDenyHidesEverything(t *testing.T) {
	s := openTestStore(t)
	clearAllowlist(t, s)
	ctx := context.Background()

	chats, err := s.ListChats(ctx, "", 20, 0, true, "last_active")
	if err != nil || len(chats) != 0 {
		t.Fatalf("ListChats with empty allowlist = %+v, %v; want nothing", chats, err)
	}
	msgs, err := s.ListMessages(ctx, ListMessagesParams{Limit: 50})
	if err != nil || len(msgs) != 0 {
		t.Fatalf("ListMessages with empty allowlist = %d msgs, %v; want none", len(msgs), err)
	}
	chat, err := s.GetChat(ctx, "447700000001@s.whatsapp.net", true)
	if err != nil || chat != nil {
		t.Fatalf("GetChat with empty allowlist = %+v, %v; want nil", chat, err)
	}
	contacts, err := s.SearchContacts(ctx, "")
	if err != nil || len(contacts) != 0 {
		t.Fatalf("SearchContacts with empty allowlist = %+v, %v; want none", contacts, err)
	}
	if _, err := s.GetMessageContext(ctx, "a3", 1, 1); err == nil {
		t.Fatal("GetMessageContext with empty allowlist: want a not-found error")
	}
}

func TestReadAllowlist_OnlyAllowlistedChatsAreReadable(t *testing.T) {
	s := openTestStore(t)
	clearAllowlist(t, s)
	ctx := context.Background()
	if err := s.AddReadAllowlist(ctx, "447700000001@s.whatsapp.net"); err != nil {
		t.Fatalf("AddReadAllowlist: %v", err)
	}

	chats, err := s.ListChats(ctx, "", 20, 0, true, "name")
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) != 1 || chats[0].JID != "447700000001@s.whatsapp.net" {
		t.Fatalf("ListChats = %+v; want only Alice", chats)
	}

	// Global search (no chat filter) and the context expansion stay inside the allowlist.
	msgs, err := s.ListMessages(ctx, ListMessagesParams{Query: "o", Limit: 50, IncludeContext: true})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("ListMessages: expected Alice's messages")
	}
	for _, m := range msgs {
		if m.ChatJID != "447700000001@s.whatsapp.net" {
			t.Fatalf("ListMessages leaked a message from %s", m.ChatJID)
		}
	}

	// Asking for a non-allowlisted chat explicitly returns nothing.
	msgs, err = s.ListMessages(ctx, ListMessagesParams{ChatJID: "123456789@g.us", Limit: 50})
	if err != nil || len(msgs) != 0 {
		t.Fatalf("ListMessages for group = %d msgs, %v; want none", len(msgs), err)
	}

	if _, err := s.GetMessageContext(ctx, "g1", 1, 1); err == nil {
		t.Fatal("GetMessageContext for a group message: want a not-found error")
	}
	if c, err := s.GetMessageContext(ctx, "a3", 1, 1); err != nil || c.Message.ID != "a3" {
		t.Fatalf("GetMessageContext for Alice = %+v, %v; want a3", c, err)
	}

	// Bob is not allowlisted, so he is not a searchable contact; Alice is.
	contacts, err := s.SearchContacts(ctx, "")
	if err != nil {
		t.Fatalf("SearchContacts: %v", err)
	}
	if len(contacts) != 1 || contacts[0].JID != "447700000001@s.whatsapp.net" {
		t.Fatalf("SearchContacts = %+v; want only Alice", contacts)
	}
}

func TestReadAllowlist_AddressBookContactNeedsAllowlistedChat(t *testing.T) {
	s := openTestStore(t)
	clearAllowlist(t, s)
	ctx := context.Background()

	got, err := s.SearchContacts(ctx, "Jane")
	if err != nil || len(got) != 0 {
		t.Fatalf("SearchContacts(Jane) before allowlisting = %+v, %v; want none", got, err)
	}
	if err := s.AddReadAllowlist(ctx, "919900000000@s.whatsapp.net"); err != nil {
		t.Fatalf("AddReadAllowlist: %v", err)
	}
	got, err = s.SearchContacts(ctx, "Jane")
	if err != nil || len(got) != 1 || got[0].Name != "Jane Smith Delhi" {
		t.Fatalf("SearchContacts(Jane) after allowlisting = %+v, %v; want Jane", got, err)
	}
}

// Bob is `447700000002@s.whatsapp.net` in the chats table and `99887766@lid` in
// the LID map; allowlisting either form must open both.
func TestReadAllowlist_LIDAndPhoneFormsAreOneChat(t *testing.T) {
	ctx := context.Background()
	for _, entry := range []string{"447700000002@s.whatsapp.net", "99887766@lid"} {
		t.Run(entry, func(t *testing.T) {
			s := openTestStore(t)
			clearAllowlist(t, s)
			if _, err := s.db.Exec(
				"INSERT INTO chats (jid, name, last_message_time) VALUES ('99887766@lid', 'Bob (lid)', '2026-01-09 09:00:00')"); err != nil {
				t.Fatalf("insert lid chat: %v", err)
			}
			if err := s.AddReadAllowlist(ctx, entry); err != nil {
				t.Fatalf("AddReadAllowlist: %v", err)
			}
			for _, jid := range []string{"447700000002@s.whatsapp.net", "99887766@lid", "99887766:7@lid"} {
				ok, err := s.IsChatAllowed(ctx, jid)
				if err != nil || !ok {
					t.Fatalf("IsChatAllowed(%s) = %v, %v; want true", jid, ok, err)
				}
			}
			chats, err := s.ListChats(ctx, "", 20, 0, false, "name")
			if err != nil {
				t.Fatalf("ListChats: %v", err)
			}
			if len(chats) != 2 {
				t.Fatalf("ListChats = %+v; want both addressing forms of Bob", chats)
			}
			if ok, _ := s.IsChatAllowed(ctx, "447700000001@s.whatsapp.net"); ok {
				t.Fatal("Alice must not become readable through Bob's allowlist entry")
			}
		})
	}
}

func TestReadAllowlist_AddStoresCanonicalFormOnce(t *testing.T) {
	s := openTestStore(t)
	clearAllowlist(t, s)
	ctx := context.Background()

	for _, jid := range []string{"99887766@lid", "447700000002@s.whatsapp.net", "99887766:3@lid"} {
		if err := s.AddReadAllowlist(ctx, jid); err != nil {
			t.Fatalf("AddReadAllowlist(%s): %v", jid, err)
		}
	}
	entries, err := s.ListReadAllowlist(ctx)
	if err != nil {
		t.Fatalf("ListReadAllowlist: %v", err)
	}
	if len(entries) != 1 || entries[0].JID != "447700000002@s.whatsapp.net" {
		t.Fatalf("entries = %+v; want one canonical phone entry", entries)
	}
	if err := s.AddReadAllowlist(ctx, "447700000002"); err == nil {
		t.Fatal("AddReadAllowlist of a bare number: want an error")
	}
}

func TestReadAllowlist_RemoveAnyForm(t *testing.T) {
	s := openTestStore(t)
	clearAllowlist(t, s)
	ctx := context.Background()
	if err := s.AddReadAllowlist(ctx, "447700000002@s.whatsapp.net"); err != nil {
		t.Fatalf("AddReadAllowlist: %v", err)
	}
	if err := s.RemoveReadAllowlist(ctx, "99887766@lid"); err != nil {
		t.Fatalf("RemoveReadAllowlist: %v", err)
	}
	if ok, err := s.IsChatAllowed(ctx, "447700000002@s.whatsapp.net"); err != nil || ok {
		t.Fatalf("IsChatAllowed after remove = %v, %v; want false", ok, err)
	}
}

func TestListAccessChats_AllowedFirstAndDanglingEntries(t *testing.T) {
	s := openTestStore(t)
	clearAllowlist(t, s)
	ctx := context.Background()
	for _, jid := range []string{"447700000002@s.whatsapp.net", "5550000@s.whatsapp.net"} {
		if err := s.AddReadAllowlist(ctx, jid); err != nil {
			t.Fatalf("AddReadAllowlist(%s): %v", jid, err)
		}
	}

	chats, err := s.ListAccessChats(ctx)
	if err != nil {
		t.Fatalf("ListAccessChats: %v", err)
	}
	if len(chats) != 5 {
		t.Fatalf("ListAccessChats = %d rows; want 4 chats + 1 entry without a chat", len(chats))
	}
	if !chats[0].Allowed || !chats[1].Allowed {
		t.Fatalf("allowed chats must come first: %+v", chats)
	}
	if chats[0].JID != "447700000002@s.whatsapp.net" {
		t.Fatalf("first row = %+v; want Bob (newest allowed chat)", chats[0])
	}
	for _, c := range chats[2:] {
		if c.Allowed {
			t.Fatalf("row %+v allowed after the non-allowed ones", c)
		}
	}
}

func TestAllowedChats_ClauseMatchesNothingWhenEmpty(t *testing.T) {
	sql, args := AllowedChats{}.clause("c.jid")
	if sql != "0 = 1" || len(args) != 0 {
		t.Fatalf("empty clause = %q %v", sql, args)
	}
}
