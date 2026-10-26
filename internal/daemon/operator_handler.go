package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sealjay/mcp-whatsapp/internal/store"
)

var (
	approvalsTmpl = template.Must(template.ParseFS(templateFS, "templates/operator_base.html.tmpl", "templates/approvals.html.tmpl"))
	accessTmpl    = template.Must(template.ParseFS(templateFS, "templates/operator_base.html.tmpl", "templates/access.html.tmpl"))
)

// recentActionsLimit is how many decided actions /approvals lists.
const recentActionsLimit = 20

// approvalBackend is what the /approvals page needs from the MCP server.
// *mcp.Server satisfies it; tests substitute a fake.
type approvalBackend interface {
	PendingActions(ctx context.Context) ([]store.PendingAction, error)
	RecentActions(ctx context.Context, limit int) ([]store.PendingAction, error)
	ApproveAction(ctx context.Context, id string) error
	RejectAction(ctx context.Context, id string) error
	TargetName(jid string) string
}

// accessBackend is what the /access page needs from the cache. *store.Store
// satisfies it.
type accessBackend interface {
	ListAccessChats(ctx context.Context, extra []store.AccessChat) ([]store.AccessChat, error)
	ReplaceReadAllowlist(ctx context.Context, jids []string) error
}

// groupsBackend lists the groups the account is in, straight from WhatsApp and
// regardless of the read allowlist. *client.Client satisfies it; it must only
// ever be wired to the operator page.
type groupsBackend interface {
	IsConnected() bool
	OperatorJoinedGroups(ctx context.Context) ([]store.AccessChat, error)
}

// operatorHandlers serves the human-only operator pages: /approvals decides
// the actions the model queued, /access manages the read allowlist.
type operatorHandlers struct {
	approvals     approvalBackend
	access        accessBackend
	groups        groupsBackend
	approvalsCSRF csrfGuard
	accessCSRF    csrfGuard
}

func newOperatorHandlers(approvals approvalBackend, access accessBackend, groups groupsBackend) *operatorHandlers {
	return &operatorHandlers{approvals: approvals, access: access, groups: groups}
}

func (h *operatorHandlers) mount(mux *http.ServeMux) {
	mux.HandleFunc("/approvals", h.handleApprovalsPage)
	mux.HandleFunc("/approvals/approve", h.handleApprove)
	mux.HandleFunc("/approvals/reject", h.handleReject)
	mux.HandleFunc("/access", h.handleAccessPage)
	mux.HandleFunc("/access/save", h.handleAccessSave)
}

// actionView is one queued or decided action as the approvals page shows it.
type actionView struct {
	ID         string
	Tool       string
	Summary    string
	TargetJID  string
	TargetName string
	Args       string
	Status     string
	Created    string
	Expires    string
	Decided    string
	Result     string
}

type approvalsPageData struct {
	CSRFToken string
	Pending   []actionView
	Recent    []actionView
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// prettyJSON indents a stored JSON document for display.
func prettyJSON(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		return "", fmt.Errorf("stored JSON is invalid: %w", err)
	}
	return buf.String(), nil
}

func (h *operatorHandlers) actionView(a store.PendingAction) (actionView, error) {
	args, err := prettyJSON(a.ArgsJSON)
	if err != nil {
		return actionView{}, err
	}
	result, err := prettyJSON(a.ResultJSON)
	if err != nil {
		return actionView{}, err
	}
	v := actionView{
		ID:        a.ID,
		Tool:      a.Tool,
		Summary:   a.Summary,
		TargetJID: a.TargetJID,
		Args:      args,
		Status:    a.Status,
		Created:   formatTime(a.CreatedAt),
		Expires:   formatTime(a.ExpiresAt),
		Decided:   formatTime(a.DecidedAt),
		Result:    result,
	}
	if a.TargetJID != "" {
		v.TargetName = h.approvals.TargetName(a.TargetJID)
	}
	return v, nil
}

func (h *operatorHandlers) views(actions []store.PendingAction) ([]actionView, error) {
	views := make([]actionView, 0, len(actions))
	for _, a := range actions {
		v, err := h.actionView(a)
		if err != nil {
			return nil, err
		}
		views = append(views, v)
	}
	return views, nil
}

// pageHeaders marks a page as uncacheable and unframeable: its buttons decide
// what the model may do.
func pageHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
}

func (h *operatorHandlers) handleApprovalsPage(w http.ResponseWriter, r *http.Request) {
	pending, err := h.approvals.PendingActions(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	recent, err := h.approvals.RecentActions(r.Context(), recentActionsLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := approvalsPageData{CSRFToken: h.approvalsCSRF.generate()}
	if data.Pending, err = h.views(pending); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if data.Recent, err = h.views(recent); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pageHeaders(w)
	if err := approvalsTmpl.ExecuteTemplate(w, "base", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// guardPost applies the checks every state-changing operator request must pass
// (POST, same origin, valid single-use CSRF token) and parses the form. It
// writes the refusal itself and returns false when the request must not run.
func guardPost(w http.ResponseWriter, r *http.Request, csrf *csrfGuard) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return false
	}
	if err := sameOriginPost(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return false
	}
	if !csrf.valid(r.FormValue("csrf_token")) {
		http.Error(w, "invalid or missing CSRF token", http.StatusForbidden)
		return false
	}
	return true
}

// decisionStatus maps a failed decision to an HTTP status.
func decisionStatus(err error) int {
	switch {
	case errors.Is(err, store.ErrActionNotFound):
		return http.StatusNotFound
	case errors.Is(err, store.ErrActionNotPending), errors.Is(err, store.ErrActionExpired):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func (h *operatorHandlers) decide(w http.ResponseWriter, r *http.Request, run func(context.Context, string) error) {
	if !guardPost(w, r, &h.approvalsCSRF) {
		return
	}
	id := r.FormValue("id")
	if id == "" {
		http.Error(w, "id: required", http.StatusBadRequest)
		return
	}
	if err := run(r.Context(), id); err != nil {
		http.Error(w, err.Error(), decisionStatus(err))
		return
	}
	http.Redirect(w, r, "/approvals", http.StatusSeeOther)
}

func (h *operatorHandlers) handleApprove(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, h.approvals.ApproveAction)
}

func (h *operatorHandlers) handleReject(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, h.approvals.RejectAction)
}

// accessRow is one chat as the access page shows it.
type accessRow struct {
	JID string
	// Phone is the "+<number>" of a phone-addressed chat, empty otherwise.
	Phone       string
	Name        string
	IsGroup     bool
	Allowed     bool
	LastMessage string
	// Search is the lowercase text the client-side filter matches against.
	Search string
}

type accessPageData struct {
	CSRFToken string
	Chats     []accessRow
	// Allowed is how many rows start out checked.
	Allowed int
	// Saved is the allowlist size reported after a save, -1 when the page was
	// not reached through one.
	Saved int
	// NotConnected is set when the live group list could not be fetched, so
	// groups without a cached message are missing from Chats.
	NotConnected bool
}

// loadAccessChats returns the chats the page offers: the cached ones plus the
// live joined groups. connected is false when WhatsApp is not connected and
// only the cache could be listed.
func (h *operatorHandlers) loadAccessChats(ctx context.Context) (chats []store.AccessChat, connected bool, err error) {
	var joined []store.AccessChat
	if h.groups.IsConnected() {
		connected = true
		if joined, err = h.groups.OperatorJoinedGroups(ctx); err != nil {
			return nil, false, err
		}
	}
	chats, err = h.access.ListAccessChats(ctx, joined)
	if err != nil {
		return nil, false, err
	}
	return chats, connected, nil
}

func (h *operatorHandlers) handleAccessPage(w http.ResponseWriter, r *http.Request) {
	chats, connected, err := h.loadAccessChats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := accessPageData{
		CSRFToken:    h.accessCSRF.generate(),
		Chats:        make([]accessRow, 0, len(chats)),
		Saved:        -1,
		NotConnected: !connected,
	}
	if saved, err := strconv.Atoi(r.URL.Query().Get("saved")); err == nil && saved >= 0 {
		data.Saved = saved
	}
	for _, c := range chats {
		row := accessRow{
			JID:         c.JID,
			Name:        c.Name,
			IsGroup:     c.IsGroup,
			Allowed:     c.Allowed,
			LastMessage: formatTime(c.LastMessageTime),
		}
		if user, ok := strings.CutSuffix(c.JID, "@s.whatsapp.net"); ok {
			row.Phone = "+" + user
		}
		row.Search = strings.ToLower(c.Name + " " + row.Phone + " " + c.JID)
		if c.Allowed {
			data.Allowed++
		}
		data.Chats = append(data.Chats, row)
	}
	pageHeaders(w)
	if err := accessTmpl.ExecuteTemplate(w, "base", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// handleAccessSave replaces the whole read allowlist with the checked chats.
// Every submitted JID must be a chat the page offers, so a forged or stale form
// cannot allowlist something the operator was never shown.
func (h *operatorHandlers) handleAccessSave(w http.ResponseWriter, r *http.Request) {
	if !guardPost(w, r, &h.accessCSRF) {
		return
	}
	chats, _, err := h.loadAccessChats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	offered := make(map[string]struct{}, len(chats))
	for _, c := range chats {
		offered[c.JID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(r.PostForm["chat_jid"]))
	var jids, unknown []string
	for _, jid := range r.PostForm["chat_jid"] {
		if _, dup := seen[jid]; dup {
			continue
		}
		seen[jid] = struct{}{}
		if _, ok := offered[jid]; !ok {
			unknown = append(unknown, jid)
			continue
		}
		jids = append(jids, jid)
	}
	if len(unknown) > 0 {
		http.Error(w, "chat_jid: not a known chat: "+strings.Join(unknown, ", "), http.StatusBadRequest)
		return
	}
	if err := h.access.ReplaceReadAllowlist(r.Context(), jids); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/access?saved="+strconv.Itoa(len(jids)), http.StatusSeeOther)
}
