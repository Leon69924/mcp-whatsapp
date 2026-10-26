package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
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

// accessBackend is what the /access page needs. *store.Store satisfies it.
type accessBackend interface {
	ListAccessChats(ctx context.Context) ([]store.AccessChat, error)
	AddReadAllowlist(ctx context.Context, jid string) error
	RemoveReadAllowlist(ctx context.Context, jid string) error
}

// operatorHandlers serves the human-only operator pages: /approvals decides
// the actions the model queued, /access manages the read allowlist.
type operatorHandlers struct {
	approvals     approvalBackend
	access        accessBackend
	approvalsCSRF csrfGuard
	accessCSRF    csrfGuard
}

func newOperatorHandlers(approvals approvalBackend, access accessBackend) *operatorHandlers {
	return &operatorHandlers{approvals: approvals, access: access}
}

func (h *operatorHandlers) mount(mux *http.ServeMux) {
	mux.HandleFunc("/approvals", h.handleApprovalsPage)
	mux.HandleFunc("/approvals/approve", h.handleApprove)
	mux.HandleFunc("/approvals/reject", h.handleReject)
	mux.HandleFunc("/access", h.handleAccessPage)
	mux.HandleFunc("/access/allow", h.handleAllow)
	mux.HandleFunc("/access/remove", h.handleRemove)
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
	JID         string
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
}

func (h *operatorHandlers) handleAccessPage(w http.ResponseWriter, r *http.Request) {
	chats, err := h.access.ListAccessChats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := accessPageData{CSRFToken: h.accessCSRF.generate(), Chats: make([]accessRow, 0, len(chats))}
	for _, c := range chats {
		data.Chats = append(data.Chats, accessRow{
			JID:         c.JID,
			Name:        c.Name,
			IsGroup:     c.IsGroup,
			Allowed:     c.Allowed,
			LastMessage: formatTime(c.LastMessageTime),
			Search:      strings.ToLower(c.Name + " " + c.JID),
		})
	}
	pageHeaders(w)
	if err := accessTmpl.ExecuteTemplate(w, "base", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (h *operatorHandlers) changeAccess(w http.ResponseWriter, r *http.Request, run func(context.Context, string) error) {
	if !guardPost(w, r, &h.accessCSRF) {
		return
	}
	jid := r.FormValue("chat_jid")
	if jid == "" {
		http.Error(w, "chat_jid: required", http.StatusBadRequest)
		return
	}
	if err := run(r.Context(), jid); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/access", http.StatusSeeOther)
}

func (h *operatorHandlers) handleAllow(w http.ResponseWriter, r *http.Request) {
	h.changeAccess(w, r, h.access.AddReadAllowlist)
}

func (h *operatorHandlers) handleRemove(w http.ResponseWriter, r *http.Request) {
	h.changeAccess(w, r, h.access.RemoveReadAllowlist)
}
