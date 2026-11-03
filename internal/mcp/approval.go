package mcp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/sealjay/mcp-whatsapp/internal/store"
)

// Tools that send, edit, delete or reconfigure are not executed when the model
// calls them. The call is validated as the handler would and then decided by a
// person: in the MCP client's confirmation form when the client supports
// elicitation (confirm.go), otherwise at /approvals — the call is persisted in
// pending_actions and answered with `pending_approval`, and the operator
// approves or rejects it there. Either decision runs the original handler
// through the same executor — once, with a fresh context, still subject to the
// rate limiter — and is recorded in pending_actions with who decided it.

// DefaultApprovalTTL is how long a queued action stays approvable.
const DefaultApprovalTTL = 24 * time.Hour

// actionExecTimeout bounds one approved action, including media upload and
// ffmpeg transcoding.
const actionExecTimeout = 5 * time.Minute

// summaryPreviewRunes caps the text preview in an action summary.
const summaryPreviewRunes = 100

// approvalPath is the operator page that lists queued actions.
const approvalPath = "/approvals"

// ApprovalConfig configures the operator-approval queue.
type ApprovalConfig struct {
	// TTL is how long a queued action stays approvable.
	TTL time.Duration
	// Notify raises a desktop notification (notify-send) per queued action.
	Notify bool
	// BaseURL is the daemon's origin (e.g. http://127.0.0.1:8765), used to
	// build the approve_url handed to the model.
	BaseURL string
}

// Option customises a Server at construction.
type Option func(*Server)

// WithApprovals sets the approval-queue configuration.
func WithApprovals(cfg ApprovalConfig) Option {
	return func(s *Server) { s.approvals = cfg }
}

// withStore points the server at a store without a client (tests).
func withStore(st *store.Store) Option {
	return func(s *Server) { s.store = st }
}

// queuedNote is appended to the description of every approval-gated tool.
const queuedNote = " Requires the user's approval. When the MCP client supports elicitation, the user is asked to confirm this exact call in the client: an accepted call runs at once and returns the action's state as get_action_status reports it (`status` `executed` or `failed`, `result` holding the output described above); a declined one returns an error and does nothing — do not repeat it unless the user asks. A client without elicitation gets the operator queue instead: the call returns `{status: \"pending_approval\", action_id, summary, approve_url}` without doing anything, the operator approves or rejects it at /approvals, and the result is then available from get_action_status."

// queuedDesc adds the approval note to a gated tool's description.
func queuedDesc(description string) string { return description + queuedNote }

// executor runs a stored action's original handler from its JSON arguments.
type executor func(ctx context.Context, rawArgs json.RawMessage) (*mcp.CallToolResult, error)

// gatedTool describes one approval-gated tool over its typed argument struct.
type gatedTool[A any] struct {
	// prepare validates the arguments exactly as the handler would, so a bad
	// call fails at once instead of waiting for the operator. It returns the
	// chat JID the action touches ("" for account-level actions) and a one-line
	// description for the summary; a non-nil result rejects the call.
	prepare func(ctx context.Context, a A) (target, detail string, reject *mcp.CallToolResult)
	// execute performs the action once the operator has approved it.
	execute func(ctx context.Context, a A) (*mcp.CallToolResult, error)
}

// registerGated registers tool so that calling it queues the action for
// operator approval instead of running it.
func registerGated[A any](s *Server, tool mcp.Tool, g gatedTool[A]) {
	s.executors[tool.Name] = func(ctx context.Context, rawArgs json.RawMessage) (*mcp.CallToolResult, error) {
		var a A
		if err := json.Unmarshal(rawArgs, &a); err != nil {
			return nil, fmt.Errorf("decode stored arguments: %w", err)
		}
		return g.execute(ctx, a)
	}
	s.mcp.AddTool(tool, mcp.NewTypedToolHandler(func(ctx context.Context, req mcp.CallToolRequest, a A) (*mcp.CallToolResult, error) {
		target, detail, reject := g.prepare(ctx, a)
		if reject != nil {
			return reject, nil
		}
		// The typed struct re-marshals to canonical JSON (fixed field order,
		// unknown fields dropped): what is stored, executed and confirmed.
		argsJSON, err := json.Marshal(a)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("marshal arguments: %v", err)), nil
		}
		call := gatedCall{tool: tool.Name, argsJSON: argsJSON, target: target, detail: detail}
		switch {
		case req.Params.RequestState != "":
			return s.decideConfirmation(ctx, call, req.Params.MultiRoundTripParams)
		case canElicit(ctx):
			return s.askConfirmation(call)
		default:
			return s.queueAction(ctx, call)
		}
	}))
}

// gatedCall is one validated call of a gated tool.
type gatedCall struct {
	tool     string
	argsJSON []byte
	// target is the chat JID the action touches, "" for account-level actions.
	target string
	// detail is the one-line description of what the action does.
	detail string
}

// pendingApproval is what the model gets back for a queued action.
type pendingApproval struct {
	Status     string `json:"status"`
	ActionID   string `json:"action_id"`
	Summary    string `json:"summary"`
	ApproveURL string `json:"approve_url"`
}

// queueAction persists the action and answers with pending_approval.
func (s *Server) queueAction(ctx context.Context, call gatedCall) (*mcp.CallToolResult, error) {
	action, err := s.newAction(ctx, rand.Text(), call)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := s.store.CreateAction(ctx, action); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	s.notifyQueued(action.Summary)
	return resultJSON(pendingApproval{
		Status:     "pending_approval",
		ActionID:   action.ID,
		Summary:    action.Summary,
		ApproveURL: s.approvals.BaseURL + approvalPath + "#" + action.ID,
	})
}

// newAction builds the pending_actions row for call under id, created now.
func (s *Server) newAction(ctx context.Context, id string, call gatedCall) (store.PendingAction, error) {
	summary, err := s.summarize(ctx, call.tool, call.target, call.detail)
	if err != nil {
		return store.PendingAction{}, err
	}
	now := time.Now().UTC()
	return store.PendingAction{
		ID:        id,
		Tool:      call.tool,
		ArgsJSON:  string(call.argsJSON),
		Summary:   summary,
		TargetJID: call.target,
		CreatedAt: now,
		ExpiresAt: now.Add(s.approvals.TTL),
	}, nil
}

// summarize builds the one-line description of a queued action. The summary is
// returned to the model, so a chat's name is included only when the model may
// read that chat; otherwise a guessed JID would reveal who it belongs to. The
// operator's /approvals page resolves the name itself.
func (s *Server) summarize(ctx context.Context, tool, target, detail string) (string, error) {
	if target == "" {
		return tool + ": " + detail, nil
	}
	label := target
	allowed, err := s.store.IsChatAllowed(ctx, target)
	if err != nil {
		return "", fmt.Errorf("read allowlist lookup failed: %w", err)
	}
	if allowed {
		if name := s.store.DisplayName(target); name != "" {
			label = name + " (" + target + ")"
		}
	}
	return fmt.Sprintf("%s -> %s: %s", tool, label, detail), nil
}

// previewText quotes a message body for a summary, collapsing whitespace and
// capping the length.
func previewText(text string) string {
	collapsed := strings.Join(strings.Fields(text), " ")
	if runes := []rune(collapsed); len(runes) > summaryPreviewRunes {
		collapsed = string(runes[:summaryPreviewRunes]) + "…"
	}
	return strconv.Quote(collapsed)
}

// notifyQueued raises a desktop notification for a queued action when
// configured. It runs asynchronously; a failure is logged, never dropped.
func (s *Server) notifyQueued(summary string) {
	if !s.approvals.Notify {
		return
	}
	go func() {
		out, err := exec.Command("notify-send", "-a", "whatsapp-mcp", "WhatsApp: approval needed", summary).CombinedOutput()
		if err != nil {
			log.Printf("whatsapp-mcp: notify-send failed: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	}()
}

// actionResult is the stored outcome of an executed action.
type actionResult struct {
	IsError bool   `json:"is_error"`
	Text    string `json:"text"`
}

// actionOutcome maps what a handler returned to the final status and the
// stored result JSON. A handler error or an error result means the action
// failed.
func actionOutcome(res *mcp.CallToolResult, runErr error) (status, resultJSON string, err error) {
	out := actionResult{}
	switch {
	case runErr != nil:
		out = actionResult{IsError: true, Text: runErr.Error()}
	case res == nil:
		out = actionResult{IsError: true, Text: "handler returned no result"}
	default:
		out.IsError = res.IsError
		var texts []string
		for _, c := range res.Content {
			if t, ok := mcp.AsTextContent(c); ok {
				texts = append(texts, t.Text)
			}
		}
		out.Text = strings.Join(texts, "\n")
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", "", fmt.Errorf("marshal action result: %w", err)
	}
	if out.IsError {
		return store.ActionFailed, string(b), nil
	}
	return store.ActionExecuted, string(b), nil
}

// ApproveAction is the operator's approval: it atomically claims the pending
// action and runs its original handler once. A handler failure is recorded on
// the action (status failed), not returned; the error is for an action that
// could not be claimed (unknown, already decided, expired) or recorded.
func (s *Server) ApproveAction(ctx context.Context, id string) error {
	action, err := s.store.ClaimAction(ctx, id, time.Now().UTC())
	if err != nil {
		return err
	}
	return s.runAction(action)
}

// runAction runs an executing action's original handler once and records the
// outcome. It is the one executor for both an operator approval and a
// confirmation in the client.
func (s *Server) runAction(action store.PendingAction) error {
	var (
		res    *mcp.CallToolResult
		runErr error
	)
	if run, ok := s.executors[action.Tool]; ok {
		runCtx, cancel := context.WithTimeout(context.Background(), actionExecTimeout)
		defer cancel()
		res, runErr = run(runCtx, json.RawMessage(action.ArgsJSON))
	} else {
		runErr = fmt.Errorf("tool: %q is not an approval-gated tool", action.Tool)
	}
	status, resultJSON, err := actionOutcome(res, runErr)
	if err != nil {
		return err
	}
	return s.store.FinishAction(context.Background(), action.ID, status, resultJSON)
}

// RejectAction is the operator's rejection: the action never runs.
func (s *Server) RejectAction(ctx context.Context, id string) error {
	return s.store.RejectAction(ctx, id, time.Now().UTC())
}

// PendingActions lists the actions awaiting the operator's decision.
func (s *Server) PendingActions(ctx context.Context) ([]store.PendingAction, error) {
	now := time.Now().UTC()
	if _, err := s.store.ExpireActions(ctx, now); err != nil {
		return nil, err
	}
	return s.store.ListPendingActions(ctx, now)
}

// RecentActions lists the most recently decided actions with their outcomes.
func (s *Server) RecentActions(ctx context.Context, limit int) ([]store.PendingAction, error) {
	if _, err := s.store.ExpireActions(ctx, time.Now().UTC()); err != nil {
		return nil, err
	}
	return s.store.ListDecidedActions(ctx, limit)
}

// TargetName resolves the display name of an action's target chat for the
// operator page.
func (s *Server) TargetName(jid string) string { return s.store.DisplayName(jid) }
