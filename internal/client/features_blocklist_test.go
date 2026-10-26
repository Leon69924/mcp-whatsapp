package client

import (
	"context"
	"encoding/json"
	"testing"

	"go.mau.fi/whatsmeow/types"

	"github.com/sealjay/mcp-whatsapp/internal/store"
)

// Blocklist methods gate on (*whatsmeow.Client).IsConnected, which is false
// when c.wa is nil — so we can exercise the "not-connected" branch cheaply
// via newDisconnectedClient() (defined in features_groups_test.go).

func TestGetBlocklist_NotConnected(t *testing.T) {
	c := newDisconnectedClient()
	_, err := c.GetBlocklist(context.Background())
	assertNotConnected(t, err)
}

func TestBlockContact_NotConnected(t *testing.T) {
	c := newDisconnectedClient()
	err := c.BlockContact(context.Background(), "447700000001")
	assertNotConnected(t, err)
}

func TestUnblockContact_NotConnected(t *testing.T) {
	c := newDisconnectedClient()
	err := c.UnblockContact(context.Background(), "447700000001")
	assertNotConnected(t, err)
}

func TestBlocklistJSON_AddsPhones(t *testing.T) {
	list := &types.Blocklist{DHash: "h", Items: []types.BlocklistItem{
		{LID: types.JID{User: "99887766", Server: types.HiddenUserServer}},
		{LID: types.JID{User: "11112222", Server: types.HiddenUserServer}},
		{
			LID: types.JID{User: "55554444", Server: types.HiddenUserServer},
			PN:  types.JID{User: "447700000003", Server: types.DefaultUserServer},
		},
	}}
	// The server-supplied PN wins; the LID map only fills items without one.
	phones := map[string]string{"99887766@lid": "447700000002", "55554444@lid": "stale"}
	b, err := json.Marshal(blocklistJSON(list, func(j string) string { return phones[j] }))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"DHash":"h","JIDs":["99887766@lid","11112222@lid","55554444@lid"],"Contacts":[{"jid":"99887766@lid","phone":"447700000002"},{"jid":"11112222@lid"},{"jid":"55554444@lid","phone":"447700000003"}]}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
}

func TestReadableBlocklist_KeepsOnlyAllowlistedEntries(t *testing.T) {
	list := &types.Blocklist{DHash: "h", Items: []types.BlocklistItem{
		{LID: types.JID{User: "99887766", Server: types.HiddenUserServer}},
		{
			LID: types.JID{User: "55554444", Server: types.HiddenUserServer},
			PN:  types.JID{User: "447700000003", Server: types.DefaultUserServer},
		},
		{LID: types.JID{User: "11112222", Server: types.HiddenUserServer}},
	}}
	allowed := store.AllowedChats{
		"99887766@lid":                {},
		"447700000003@s.whatsapp.net": {},
	}
	got := readableBlocklist(list, allowed)
	if got.DHash != "h" || len(got.Items) != 2 {
		t.Fatalf("readableBlocklist = %+v; want the two allowlisted items", got)
	}
	if got.Items[0].LID.User != "99887766" || got.Items[1].LID.User != "55554444" {
		t.Fatalf("readableBlocklist kept the wrong items: %+v", got.Items)
	}
	if empty := readableBlocklist(list, store.AllowedChats{}); len(empty.Items) != 0 {
		t.Fatalf("empty allowlist kept %d items", len(empty.Items))
	}
}
