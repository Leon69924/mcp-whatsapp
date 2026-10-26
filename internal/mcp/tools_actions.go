package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/sealjay/mcp-whatsapp/internal/store"
)

// registerActionTools wires the tool that reports on queued actions.
func (s *Server) registerActionTools() {
	s.registerGetActionStatus()
}

type getActionStatusArgs struct {
	ActionID string `json:"action_id"`
}

// actionStatus is the get_action_status payload.
type actionStatus struct {
	ActionID  string          `json:"action_id"`
	Tool      string          `json:"tool"`
	Status    string          `json:"status"`
	Summary   string          `json:"summary"`
	TargetJID string          `json:"target_jid,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	ExpiresAt time.Time       `json:"expires_at"`
	DecidedAt *time.Time      `json:"decided_at,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	// ResultWithheld explains why Result carries no text.
	ResultWithheld string `json:"result_withheld,omitempty"`
}

// redactedResult is all that is revealed of an outcome whose text may describe
// a chat the model may not read.
type redactedResult struct {
	IsError bool `json:"is_error"`
}

func (s *Server) registerGetActionStatus() {
	tool := mcp.NewTool("get_action_status",
		mcp.WithDescription(offlineSafePrefix+"Report the state of an action that a gated tool queued for operator approval. Read-only; no side effects. Poll it after a tool returned `pending_approval`; the operator decides at /approvals. Returns a JSON object `{action_id, tool, status, summary, target_jid?, created_at, expires_at, decided_at?, result?}` where `status` is `pending`, `rejected`, `expired`, `executing`, `executed`, or `failed`, and `result` (`{is_error, text}`) appears once the action has run."),
		mcp.WithString("action_id", mcp.Required(), mcp.Description("`action_id` returned by the gated tool call")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	)
	s.mcp.AddTool(tool, mcp.NewTypedToolHandler(func(ctx context.Context, _ mcp.CallToolRequest, a getActionStatusArgs) (*mcp.CallToolResult, error) {
		if r := requireNonEmpty("action_id", a.ActionID); r != nil {
			return r, nil
		}
		if _, err := s.store.ExpireActions(ctx, time.Now().UTC()); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		action, err := s.store.GetAction(ctx, a.ActionID)
		if errors.Is(err, store.ErrActionNotFound) {
			return mcp.NewToolResultError("action_id: not found"), nil
		}
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		out := actionStatus{
			ActionID:  action.ID,
			Tool:      action.Tool,
			Status:    action.Status,
			Summary:   action.Summary,
			TargetJID: action.TargetJID,
			CreatedAt: action.CreatedAt,
			ExpiresAt: action.ExpiresAt,
		}
		if !action.DecidedAt.IsZero() {
			out.DecidedAt = &action.DecidedAt
		}
		if action.ResultJSON != "" {
			if err := s.fillResult(ctx, &out, action); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
		}
		return resultJSON(out)
	}))
}

// fillResult attaches the execution result to the status. The result text can
// describe chats and members (group info, invite links, errors naming a group),
// so it is shown only for an action whose target chat the model may read;
// otherwise only success or failure is revealed.
func (s *Server) fillResult(ctx context.Context, out *actionStatus, action store.PendingAction) error {
	readable := false
	if action.TargetJID != "" {
		allowed, err := s.store.IsChatAllowed(ctx, action.TargetJID)
		if err != nil {
			return fmt.Errorf("read allowlist lookup failed: %w", err)
		}
		readable = allowed
	}
	if readable {
		out.Result = json.RawMessage(action.ResultJSON)
		return nil
	}
	var full actionResult
	if err := json.Unmarshal([]byte(action.ResultJSON), &full); err != nil {
		return fmt.Errorf("decode stored result: %w", err)
	}
	redacted, err := json.Marshal(redactedResult{IsError: full.IsError})
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	out.Result = redacted
	out.ResultWithheld = "the action's target is not a chat on the read allowlist; the operator can read the full result at /approvals"
	return nil
}
