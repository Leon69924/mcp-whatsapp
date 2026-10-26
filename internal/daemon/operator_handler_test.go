package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	chats   []store.AccessChat
	added   []string
	removed []string
}

func (f *fakeAccess) ListAccessChats(context.Context) ([]store.AccessChat, error) {
	return f.chats, nil
}
func (f *fakeAccess) AddReadAllowlist(_ context.Context, jid string) error {
	f.added = append(f.added, jid)
	return nil
}
func (f *fakeAccess) RemoveReadAllowlist(_ context.Context, jid string) error {
	f.removed = append(f.removed, jid)
	return nil
}

func newOperatorTest() (*operatorHandlers, *fakeApprovals, *fakeAccess) {
	ap := &fakeApprovals{pending: []store.PendingAction{{
		ID: "act1", Tool: "send_message", Summary: "send_message -> x: hi",
		TargetJID: "447700000001@s.whatsapp.net",
		ArgsJSON:  `{"recipient":"447700000001","message":"the full message text"}`,
		Status:    store.ActionPending,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}}}
	ac := &fakeAccess{}
	return newOperatorHandlers(ap, ac), ap, ac
}

func get(h http.HandlerFunc, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

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
	h, ap, _ := newOperatorTest()
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
		"the full message text", "447700000001@s.whatsapp.net", "Alice", "send_message -> x: hi",
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
	h, ap, _ := newOperatorTest()
	form := func(token string) url.Values { return url.Values{"id": {"act1"}, "csrf_token": {token}} }

	if w := post(h.handleApprove, "/approvals/approve", form("wrong"), nil); w.Code != http.StatusForbidden {
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

	same := map[string]string{"Origin": "http://example.com", "Sec-Fetch-Site": "same-origin"}
	w := post(h.handleApprove, "/approvals/approve", form(token), same)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/approvals" {
		t.Fatalf("approve: status = %d location = %q", w.Code, w.Header().Get("Location"))
	}
	if len(ap.approved) != 1 || ap.approved[0] != "act1" {
		t.Fatalf("approved = %v", ap.approved)
	}

	// The token was consumed: replaying the same form must not approve again.
	if w := post(h.handleApprove, "/approvals/approve", form(token), nil); w.Code != http.StatusForbidden {
		t.Fatalf("replayed token: status = %d; want 403", w.Code)
	}
}

func TestApprove_GETIsRefused(t *testing.T) {
	h, ap, _ := newOperatorTest()
	if w := get(h.handleApprove, "/approvals/approve?id=act1"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET approve: status = %d; want 405", w.Code)
	}
	if len(ap.approved) != 0 {
		t.Fatal("GET must never approve")
	}
}

func TestDecision_ErrorsMapToHTTPStatus(t *testing.T) {
	h, ap, _ := newOperatorTest()
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
		w := post(h.handleReject, "/approvals/reject", url.Values{"id": {"act1"}, "csrf_token": {token}}, nil)
		if w.Code != tc.want {
			t.Errorf("%v: status = %d; want %d", tc.err, w.Code, tc.want)
		}
	}
}

func TestAccessPage_ListsChatsWithSearchAndButtons(t *testing.T) {
	h, _, ac := newOperatorTest()
	ac.chats = []store.AccessChat{
		{JID: "447700000001@s.whatsapp.net", Name: "Alice", Allowed: true},
		{JID: "123456789@g.us", Name: "Project Team", IsGroup: true},
	}
	w := get(h.handleAccessPage, "/access")
	body := w.Body.String()
	for _, want := range []string{
		`id="filter"`, "Alice", "Project Team", "group", "direct",
		`action="/access/remove"`, `action="/access/allow"`, `href="/approvals"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("access page missing %q", want)
		}
	}
	if strings.Index(body, "Alice") > strings.Index(body, "Project Team") {
		t.Error("allowed chats must be listed first")
	}
}

func TestAccessChanges_RequireTokenAndOrigin(t *testing.T) {
	h, _, ac := newOperatorTest()
	jid := "447700000001@s.whatsapp.net"

	if w := post(h.handleAllow, "/access/allow", url.Values{"chat_jid": {jid}}, nil); w.Code != http.StatusForbidden {
		t.Fatalf("no token: status = %d; want 403", w.Code)
	}
	token := h.accessCSRF.generate()
	if w := post(h.handleAllow, "/access/allow", url.Values{"chat_jid": {jid}, "csrf_token": {token}},
		map[string]string{"Origin": "http://evil.example"}); w.Code != http.StatusForbidden {
		t.Fatalf("foreign Origin: status = %d; want 403", w.Code)
	}
	if len(ac.added) != 0 {
		t.Fatalf("refused requests allowed %v", ac.added)
	}

	if w := post(h.handleAllow, "/access/allow", url.Values{"chat_jid": {jid}, "csrf_token": {token}}, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("allow: status = %d", w.Code)
	}
	token = h.accessCSRF.generate()
	if w := post(h.handleRemove, "/access/remove", url.Values{"chat_jid": {jid}, "csrf_token": {token}}, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("remove: status = %d", w.Code)
	}
	if len(ac.added) != 1 || len(ac.removed) != 1 {
		t.Fatalf("added=%v removed=%v", ac.added, ac.removed)
	}
}

func TestRequiresAuth_CoversOperatorPages(t *testing.T) {
	for path, want := range map[string]bool{
		"/mcp": true, "/pair": true, "/pair/reset": true,
		"/approvals": true, "/approvals/approve": true,
		"/access": true, "/access/allow": true,
		"/accessory": false, "/other": false,
	} {
		if got := requiresAuth(path); got != want {
			t.Errorf("requiresAuth(%q) = %v; want %v", path, got, want)
		}
	}
}

func TestAuthMiddleware_ProtectsOperatorPages(t *testing.T) {
	h, _, _ := newOperatorTest()
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
