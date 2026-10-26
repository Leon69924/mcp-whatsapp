package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sealjay/mcp-whatsapp/internal/store"
)

type fakeApprovals struct {
	pending  []store.PendingAction
	recent   []store.PendingAction
	approved []string
	rejected []string
	err      error
}

func (f *fakeApprovals) PendingActions(context.Context) ([]store.PendingAction, error) {
	return f.pending, nil
}
func (f *fakeApprovals) RecentActions(context.Context, int) ([]store.PendingAction, error) {
	return f.recent, nil
}
func (f *fakeApprovals) ApproveAction(_ context.Context, id string) error {
	f.approved = append(f.approved, id)
	return f.err
}
func (f *fakeApprovals) RejectAction(_ context.Context, id string) error {
	f.rejected = append(f.rejected, id)
	return f.err
}
func (f *fakeApprovals) TargetName(string) string { return "Alice" }

type fakeAccess struct {
	chats    []store.AccessChat
	replaced [][]string
	err      error
}

func (f *fakeAccess) ListAccessChats(_ context.Context, extra []store.AccessChat) ([]store.AccessChat, error) {
	return append(append([]store.AccessChat(nil), f.chats...), extra...), nil
}
func (f *fakeAccess) ReplaceReadAllowlist(_ context.Context, jids []string) error {
	f.replaced = append(f.replaced, jids)
	return f.err
}

type fakeGroups struct {
	connected bool
	groups    []store.AccessChat
}

func (f *fakeGroups) IsConnected() bool { return f.connected }
func (f *fakeGroups) OperatorJoinedGroups(context.Context) ([]store.AccessChat, error) {
	return f.groups, nil
}

func newOperatorTest() (*operatorHandlers, *fakeApprovals, *fakeAccess, *fakeGroups) {
	ap := &fakeApprovals{pending: []store.PendingAction{{
		ID: "act1", Tool: "send_message", Summary: "send_message -> x: hi",
		TargetJID: "447700000001@s.whatsapp.net",
		ArgsJSON:  `{"recipient":"447700000001","message":"the full message text"}`,
		Status:    store.ActionPending,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}}}
	ac := &fakeAccess{}
	gr := &fakeGroups{connected: true}
	return newOperatorHandlers(ap, ac, gr), ap, ac, gr
}

func get(h http.HandlerFunc, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// sameOriginHeaders are the provenance headers a browser sends for a form
// posted from a page this daemon served (httptest requests use example.com).
var sameOriginHeaders = map[string]string{"Origin": "http://example.com", "Sec-Fetch-Site": "same-origin"}

func post(h http.HandlerFunc, path string, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

func TestApprovalsPage_ShowsFullDetail(t *testing.T) {
	h, ap, _, _ := newOperatorTest()
	ap.recent = []store.PendingAction{{
		ID: "old", Tool: "send_file", Summary: "send_file done", Status: store.ActionFailed,
		ArgsJSON: `{}`, ResultJSON: `{"is_error":true,"text":"rate limited"}`, DecidedAt: time.Now(),
	}}
	w := get(h.handleApprovalsPage, "/approvals")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"the full message text", "447700000001@s.whatsapp.net", "Alice", "send_message -&gt; x: hi",
		`action="/approvals/approve"`, `action="/approvals/reject"`, `href="/access"`,
		`name="csrf_token"`, "rate limited", "failed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("approvals page missing %q", want)
		}
	}
	if w.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("approvals page must not be frameable")
	}
}

func TestApprove_RunsOnlyWithTokenAndSameOrigin(t *testing.T) {
	h, ap, _, _ := newOperatorTest()
	form := func(token string) url.Values { return url.Values{"id": {"act1"}, "csrf_token": {token}} }

	if w := post(h.handleApprove, "/approvals/approve", form("wrong"), sameOriginHeaders); w.Code != http.StatusForbidden {
		t.Fatalf("bad token: status = %d; want 403", w.Code)
	}

	token := h.approvalsCSRF.generate()
	cross := map[string]string{"Origin": "http://evil.example"}
	if w := post(h.handleApprove, "/approvals/approve", form(token), cross); w.Code != http.StatusForbidden {
		t.Fatalf("foreign Origin: status = %d; want 403", w.Code)
	}
	site := map[string]string{"Sec-Fetch-Site": "cross-site"}
	if w := post(h.handleApprove, "/approvals/approve", form(token), site); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site fetch: status = %d; want 403", w.Code)
	}
	if len(ap.approved) != 0 {
		t.Fatalf("refused requests approved %v", ap.approved)
	}

	noProvenance := map[string]string{}
	if w := post(h.handleApprove, "/approvals/approve", form(token), noProvenance); w.Code != http.StatusForbidden {
		t.Fatalf("no provenance headers: status = %d; want 403", w.Code)
	}
	if len(ap.approved) != 0 {
		t.Fatalf("request without provenance approved %v", ap.approved)
	}

	w := post(h.handleApprove, "/approvals/approve", form(token), sameOriginHeaders)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/approvals" {
		t.Fatalf("approve: status = %d location = %q", w.Code, w.Header().Get("Location"))
	}
	if len(ap.approved) != 1 || ap.approved[0] != "act1" {
		t.Fatalf("approved = %v", ap.approved)
	}

	// The token was consumed: replaying the same form must not approve again.
	if w := post(h.handleApprove, "/approvals/approve", form(token), sameOriginHeaders); w.Code != http.StatusForbidden {
		t.Fatalf("replayed token: status = %d; want 403", w.Code)
	}
}

func TestApprove_GETIsRefused(t *testing.T) {
	h, ap, _, _ := newOperatorTest()
	if w := get(h.handleApprove, "/approvals/approve?id=act1"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET approve: status = %d; want 405", w.Code)
	}
	if len(ap.approved) != 0 {
		t.Fatal("GET must never approve")
	}
}

func TestDecision_ErrorsMapToHTTPStatus(t *testing.T) {
	h, ap, _, _ := newOperatorTest()
	cases := []struct {
		err  error
		want int
	}{
		{store.ErrActionNotFound, http.StatusNotFound},
		{store.ErrActionNotPending, http.StatusConflict},
		{store.ErrActionExpired, http.StatusConflict},
	}
	for _, tc := range cases {
		ap.err = tc.err
		token := h.approvalsCSRF.generate()
		w := post(h.handleReject, "/approvals/reject", url.Values{"id": {"act1"}, "csrf_token": {token}}, sameOriginHeaders)
		if w.Code != tc.want {
			t.Errorf("%v: status = %d; want %d", tc.err, w.Code, tc.want)
		}
	}
}

func TestAccessPage_RendersCheckboxPicker(t *testing.T) {
	h, _, ac, _ := newOperatorTest()
	ac.chats = []store.AccessChat{
		{JID: "447700000001@s.whatsapp.net", Name: "Alice", Allowed: true, LastMessageTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)},
		{JID: "123456789@g.us", Name: "Project Team", IsGroup: true},
		{JID: "447700000009@s.whatsapp.net"},
	}
	w := get(h.handleAccessPage, "/access")
	body := w.Body.String()
	for _, want := range []string{
		`action="/access/save"`, `name="csrf_token"`, `id="filter"`, `id="type"`, `id="only-selected"`,
		`id="select-visible"`, `id="clear-visible"`, `id="counter"`,
		`name="chat_jid" value="447700000001@s.whatsapp.net" checked`,
		`name="chat_jid" value="123456789@g.us"`,
		"447700000001<br>", "(no name)", "group", "direct", "2026-01-02 03:04:05", `href="/approvals"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("access page missing %q", want)
		}
	}
	if strings.Contains(body, `name="chat_jid" value="123456789@g.us" checked`) {
		t.Error("a chat that is not allowlisted must start unchecked")
	}
	if strings.Contains(body, "disabled") {
		t.Error("filters must hide rows, never disable their checkboxes: disabled boxes are not submitted")
	}
	for _, gone := range []string{"/access/allow", "/access/remove", "not connected"} {
		if strings.Contains(body, gone) {
			t.Errorf("access page must not contain %q", gone)
		}
	}
	if strings.Index(body, "Alice") > strings.Index(body, "Project Team") {
		t.Error("allowed chats must be listed first")
	}
}

func TestAccessPage_ListsJoinedGroupsWithoutCachedMessages(t *testing.T) {
	h, _, ac, gr := newOperatorTest()
	ac.chats = []store.AccessChat{{JID: "447700000001@s.whatsapp.net", Name: "Alice"}}
	gr.groups = []store.AccessChat{{JID: "555000111@g.us", Name: "Silent group", IsGroup: true}}
	body := get(h.handleAccessPage, "/access").Body.String()
	if !strings.Contains(body, `value="555000111@g.us"`) || !strings.Contains(body, "Silent group") {
		t.Fatalf("joined group without cached messages missing:\n%s", body)
	}
	if strings.Contains(body, "WhatsApp is not connected") {
		t.Error("connected page must not show the not-connected notice")
	}
}

func TestAccessPage_NotConnectedShowsNotice(t *testing.T) {
	h, _, ac, gr := newOperatorTest()
	gr.connected = false
	gr.groups = []store.AccessChat{{JID: "555000111@g.us", Name: "Silent group", IsGroup: true}}
	ac.chats = []store.AccessChat{{JID: "447700000001@s.whatsapp.net", Name: "Alice"}}
	body := get(h.handleAccessPage, "/access").Body.String()
	if !strings.Contains(body, "WhatsApp is not connected") || !strings.Contains(body, "no cached message") {
		t.Errorf("not-connected notice missing:\n%s", body)
	}
	if !strings.Contains(body, "Alice") {
		t.Error("cached chats must still be listed")
	}
	if strings.Contains(body, "Silent group") {
		t.Error("the live group list must not be consulted while disconnected")
	}
}

func TestAccessPage_ShowsSavedConfirmation(t *testing.T) {
	h, _, _, _ := newOperatorTest()
	w := httptest.NewRecorder()
	h.handleAccessPage(w, httptest.NewRequest(http.MethodGet, "/access?saved=3", nil))
	if !strings.Contains(w.Body.String(), "Saved: 3 chats allowed") {
		t.Errorf("confirmation missing:\n%s", w.Body.String())
	}
	if strings.Contains(get(h.handleAccessPage, "/access").Body.String(), "Saved:") {
		t.Error("confirmation shown without a save")
	}
}

func TestAccessSave_RequiresTokenAndOrigin(t *testing.T) {
	h, _, ac, _ := newOperatorTest()
	ac.chats = []store.AccessChat{{JID: "447700000001@s.whatsapp.net", Name: "Alice"}}
	form := func(token string) url.Values {
		return url.Values{"chat_jid": {"447700000001@s.whatsapp.net"}, "csrf_token": {token}}
	}

	if w := post(h.handleAccessSave, "/access/save", url.Values{"chat_jid": {"447700000001@s.whatsapp.net"}}, sameOriginHeaders); w.Code != http.StatusForbidden {
		t.Fatalf("no token: status = %d; want 403", w.Code)
	}
	token := h.accessCSRF.generate()
	if w := post(h.handleAccessSave, "/access/save", form(token), map[string]string{"Origin": "http://evil.example"}); w.Code != http.StatusForbidden {
		t.Fatalf("foreign Origin: status = %d; want 403", w.Code)
	}
	if w := post(h.handleAccessSave, "/access/save", form(token), nil); w.Code != http.StatusForbidden {
		t.Fatalf("no provenance headers: status = %d; want 403", w.Code)
	}
	if w := get(h.handleAccessSave, "/access/save"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: status = %d; want 405", w.Code)
	}
	if len(ac.replaced) != 0 {
		t.Fatalf("refused requests changed the allowlist: %v", ac.replaced)
	}

	w := post(h.handleAccessSave, "/access/save", form(token), sameOriginHeaders)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/access?saved=1" {
		t.Fatalf("save: status = %d, Location = %q", w.Code, w.Header().Get("Location"))
	}
	if w := post(h.handleAccessSave, "/access/save", form(token), sameOriginHeaders); w.Code != http.StatusForbidden {
		t.Fatalf("replayed token: status = %d; want 403", w.Code)
	}
	if len(ac.replaced) != 1 {
		t.Fatalf("replaced = %v; want exactly one replacement", ac.replaced)
	}
}

func TestAccessSave_ReplacesWithCheckedChats(t *testing.T) {
	h, _, ac, gr := newOperatorTest()
	ac.chats = []store.AccessChat{{JID: "447700000001@s.whatsapp.net", Allowed: true}}
	gr.groups = []store.AccessChat{{JID: "555000111@g.us", IsGroup: true}}

	token := h.accessCSRF.generate()
	w := post(h.handleAccessSave, "/access/save", url.Values{
		"csrf_token": {token},
		"chat_jid":   {"555000111@g.us", "555000111@g.us"},
	}, sameOriginHeaders)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/access?saved=1" {
		t.Fatalf("status = %d, Location = %q", w.Code, w.Header().Get("Location"))
	}
	if !reflect.DeepEqual(ac.replaced, [][]string{{"555000111@g.us"}}) {
		t.Fatalf("replaced = %v; want the one checked group, deduplicated", ac.replaced)
	}

	token = h.accessCSRF.generate()
	w = post(h.handleAccessSave, "/access/save", url.Values{"csrf_token": {token}}, sameOriginHeaders)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/access?saved=0" {
		t.Fatalf("empty save: status = %d, Location = %q", w.Code, w.Header().Get("Location"))
	}
	if len(ac.replaced) != 2 || len(ac.replaced[1]) != 0 {
		t.Fatalf("replaced = %v; an empty selection must replace the list with nothing", ac.replaced)
	}
}

func TestAccessSave_UnknownJIDIsRejectedWithoutChange(t *testing.T) {
	h, _, ac, _ := newOperatorTest()
	ac.chats = []store.AccessChat{{JID: "447700000001@s.whatsapp.net"}}

	token := h.accessCSRF.generate()
	w := post(h.handleAccessSave, "/access/save", url.Values{
		"csrf_token": {token},
		"chat_jid":   {"447700000001@s.whatsapp.net", "999@g.us", "888@s.whatsapp.net"},
	}, sameOriginHeaders)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", w.Code)
	}
	for _, jid := range []string{"999@g.us", "888@s.whatsapp.net"} {
		if !strings.Contains(w.Body.String(), jid) {
			t.Errorf("error %q does not name %s", w.Body.String(), jid)
		}
	}
	if len(ac.replaced) != 0 {
		t.Fatalf("a rejected save changed the allowlist: %v", ac.replaced)
	}
}

func TestOldPerRowAccessEndpointsAreGone(t *testing.T) {
	h, _, _, _ := newOperatorTest()
	mux := http.NewServeMux()
	h.mount(mux)
	for _, path := range []string{"/access/allow", "/access/remove"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("POST %s: status = %d; want 404", path, w.Code)
		}
	}
}

func TestRequiresAuth_CoversOperatorPages(t *testing.T) {
	for path, want := range map[string]bool{
		"/mcp": true, "/pair": true, "/pair/reset": true,
		"/approvals": true, "/approvals/approve": true,
		"/access": true, "/access/save": true,
		"/accessory": false, "/other": false,
	} {
		if got := requiresAuth(path); got != want {
			t.Errorf("requiresAuth(%q) = %v; want %v", path, got, want)
		}
	}
}

func TestAuthMiddleware_ProtectsOperatorPages(t *testing.T) {
	h, _, _, _ := newOperatorTest()
	mux := http.NewServeMux()
	h.mount(mux)
	handler := authMiddleware(mux, "secret")

	for _, path := range []string{"/approvals", "/access"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s without token: status = %d; want 401", path, w.Code)
		}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s with token: status = %d; want 200", path, w.Code)
		}
	}
}
