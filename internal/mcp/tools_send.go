package mcp

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"go.mau.fi/whatsmeow/types"

	"github.com/sealjay/mcp-whatsapp/internal/client"
	"github.com/sealjay/mcp-whatsapp/internal/media"
)

// registerSendTools wires outbound-message tools: send_message, send_file,
// send_audio_message, send_reaction, send_reply (all queued for operator
// approval) and send_typing (limited to allowlisted chats).
func (s *Server) registerSendTools() {
	s.registerSendMessage()
	s.registerSendFile()
	s.registerSendAudioMessage()
	s.registerSendReaction()
	s.registerSendReply()
	s.registerSendTyping()
}

// -- send_message -----------------------------------------------------------

type sendMessageArgs struct {
	Recipient    string `json:"recipient"`
	Message      string `json:"message"`
	MarkChatRead bool   `json:"mark_chat_read,omitempty"`
}

func (s *Server) registerSendMessage() {
	tool := mcp.NewTool("send_message",
		mcp.WithDescription(queuedDesc("Send a new WhatsApp text message to a person or group; recipients see it as a fresh message from the paired account and the row is also stored in the local cache. Reversible via delete_message (revoke) or edit_message (correct text); to quote a previous message use send_reply, for emoji acknowledgement use send_reaction. Returns a JSON object `{Success, Message, ID}` where `ID` is the WhatsApp message ID on success.")),
		mcp.WithString("recipient", mcp.Required(), mcp.Description(recipientDesc)),
		mcp.WithString("message", mcp.Required(), mcp.Description("message body text")),
		mcp.WithBoolean("mark_chat_read", mcp.DefaultBool(false), mcp.Description("if true, also ack recent incoming messages in the chat to clear the unread badge (defaults to false)")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
	registerGated(s, tool, gatedTool[sendMessageArgs]{
		prepare: func(ctx context.Context, a sendMessageArgs) (string, string, *mcp.CallToolResult) {
			target, reject := s.prepareRecipient(ctx, a.Recipient, a.MarkChatRead)
			if reject != nil {
				return "", "", reject
			}
			return target, "text message " + previewText(a.Message), nil
		},
		execute: func(ctx context.Context, a sendMessageArgs) (*mcp.CallToolResult, error) {
			r := s.client.Send(ctx, a.Recipient, a.Message)
			s.maybeMarkChatRead(ctx, r, a.Recipient, a.MarkChatRead)
			return sendOutcome(r)
		},
	})
}

// -- send_file --------------------------------------------------------------

type sendFileArgs struct {
	Recipient    string `json:"recipient"`
	MediaPath    string `json:"media_path"`
	Caption      string `json:"caption,omitempty"`
	MarkChatRead bool   `json:"mark_chat_read,omitempty"`
	ViewOnce     bool   `json:"view_once,omitempty"`
}

func (s *Server) registerSendFile() {
	tool := mcp.NewTool("send_file",
		mcp.WithDescription(queuedDesc("Upload and send a picture, video, document, or raw audio attachment via WhatsApp; the recipient sees a media message and the outgoing row is persisted to the local cache. Reversible via delete_message (revoke). For voice notes use send_audio_message (which transcodes to ogg/opus); for plain text use send_message. Returns a JSON object `{Success, Message, ID}` where `ID` is the WhatsApp message ID on success.")),
		mcp.WithString("recipient", mcp.Required(), mcp.Description(recipientDesc)),
		mcp.WithString("media_path", mcp.Required(), mcp.Description("absolute path to the media file; must sit under the configured media root (`WHATSAPP_MCP_MEDIA_ROOT`, default `<store>/uploads/`)")),
		mcp.WithString("caption", mcp.Description("optional caption for image/video/document submessages; ignored for raw audio")),
		mcp.WithBoolean("mark_chat_read", mcp.DefaultBool(false), mcp.Description("if true, also ack recent incoming messages in the chat to clear the unread badge (defaults to false)")),
		mcp.WithBoolean("view_once", mcp.DefaultBool(false), mcp.Description("if true, mark image/video/audio submessages as view-once; silently ignored for documents (defaults to false)")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
	registerGated(s, tool, gatedTool[sendFileArgs]{
		prepare: func(ctx context.Context, a sendFileArgs) (string, string, *mcp.CallToolResult) {
			if a.Recipient == "" || a.MediaPath == "" {
				return "", "", mcp.NewToolResultError("recipient and media_path are required")
			}
			safePath, err := s.client.ValidateMediaPath(a.MediaPath)
			if err != nil {
				return "", "", mcp.NewToolResultError(err.Error())
			}
			target, reject := s.prepareRecipient(ctx, a.Recipient, a.MarkChatRead)
			if reject != nil {
				return "", "", reject
			}
			detail := "file " + safePath
			if a.Caption != "" {
				detail += " with caption " + previewText(a.Caption)
			}
			if a.ViewOnce {
				detail += " (view once)"
			}
			return target, detail, nil
		},
		execute: func(ctx context.Context, a sendFileArgs) (*mcp.CallToolResult, error) {
			safePath, err := s.client.ValidateMediaPath(a.MediaPath)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			r := s.client.SendMediaWithOptions(ctx, client.SendMediaOptions{
				Recipient: a.Recipient,
				Caption:   a.Caption,
				MediaPath: safePath,
				ViewOnce:  a.ViewOnce,
			})
			s.maybeMarkChatRead(ctx, r, a.Recipient, a.MarkChatRead)
			return sendOutcome(r)
		},
	})
}

// -- send_audio_message -----------------------------------------------------

type sendAudioArgs struct {
	Recipient    string `json:"recipient"`
	MediaPath    string `json:"media_path"`
	MarkChatRead bool   `json:"mark_chat_read,omitempty"`
	ViewOnce     bool   `json:"view_once,omitempty"`
}

func (s *Server) registerSendAudioMessage() {
	tool := mcp.NewTool("send_audio_message",
		mcp.WithDescription(queuedDesc("Send an audio file as a WhatsApp voice note (waveform UI, push-to-play); non-ogg inputs are transcoded via ffmpeg before upload. Reversible via delete_message (revoke). Use send_file when you want the audio delivered as a regular attachment instead of a voice note. Prerequisites: ffmpeg must be on PATH for non-ogg inputs. Returns a JSON object `{Success, Message, ID}` where `ID` is the WhatsApp message ID on success.")),
		mcp.WithString("recipient", mcp.Required(), mcp.Description(recipientDesc)),
		mcp.WithString("media_path", mcp.Required(), mcp.Description("absolute path to the audio file; must sit under the configured media root (`WHATSAPP_MCP_MEDIA_ROOT`, default `<store>/uploads/`)")),
		mcp.WithBoolean("mark_chat_read", mcp.DefaultBool(false), mcp.Description("if true, also ack recent incoming messages in the chat to clear the unread badge (defaults to false)")),
		mcp.WithBoolean("view_once", mcp.DefaultBool(false), mcp.Description("if true, mark the voice note as view-once (defaults to false)")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
	registerGated(s, tool, gatedTool[sendAudioArgs]{
		prepare: func(ctx context.Context, a sendAudioArgs) (string, string, *mcp.CallToolResult) {
			if a.Recipient == "" || a.MediaPath == "" {
				return "", "", mcp.NewToolResultError("recipient and media_path are required")
			}
			safePath, err := s.client.ValidateMediaPath(a.MediaPath)
			if err != nil {
				return "", "", mcp.NewToolResultError(err.Error())
			}
			target, reject := s.prepareRecipient(ctx, a.Recipient, a.MarkChatRead)
			if reject != nil {
				return "", "", reject
			}
			detail := "voice note " + safePath
			if a.ViewOnce {
				detail += " (view once)"
			}
			return target, detail, nil
		},
		execute: func(ctx context.Context, a sendAudioArgs) (*mcp.CallToolResult, error) {
			safePath, err := s.client.ValidateMediaPath(a.MediaPath)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			path := safePath
			if !strings.HasSuffix(strings.ToLower(path), ".ogg") {
				converted, err := media.ConvertToOpusOgg(ctx, path)
				if err != nil {
					return mcp.NewToolResultError(fmt.Sprintf("audio conversion failed: %v (install ffmpeg, or call send_file for raw audio)", err)), nil
				}
				defer os.Remove(converted)
				path = converted
			}
			r := s.client.SendMediaWithOptions(ctx, client.SendMediaOptions{
				Recipient: a.Recipient,
				MediaPath: path,
				ViewOnce:  a.ViewOnce,
			})
			s.maybeMarkChatRead(ctx, r, a.Recipient, a.MarkChatRead)
			return sendOutcome(r)
		},
	})
}

// -- send_reaction ----------------------------------------------------------

type sendReactionArgs struct {
	ChatJID   string `json:"chat_jid"`
	MessageID string `json:"message_id"`
	SenderJID string `json:"sender_jid,omitempty"`
	Emoji     string `json:"emoji"`
}

func (s *Server) registerSendReaction() {
	tool := mcp.NewTool("send_reaction",
		mcp.WithDescription(queuedDesc("Add or replace an emoji reaction on an existing message; recipients see the small emoji badge attached to the original message. Reversible by calling again with an empty `emoji` string (clears the reaction); for a fresh message use send_message and for a quoted reply use send_reply. Returns the plain-text string `Reaction sent` on success.")),
		mcp.WithString("chat_jid", mcp.Required(), mcp.Description(jidDesc)),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("WhatsApp message ID of the target message (use `message_id` from list_messages)")),
		mcp.WithString("sender_jid", mcp.Description("JID of the original sender; required in group chats, omit in 1:1 chats ("+jidDesc+")")),
		mcp.WithString("emoji", mcp.Description("single emoji to react with; pass an empty string to clear an existing reaction")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
	)
	registerGated(s, tool, gatedTool[sendReactionArgs]{
		prepare: func(_ context.Context, a sendReactionArgs) (string, string, *mcp.CallToolResult) {
			target, reject := prepareMessageRef(a.ChatJID, a.MessageID, a.SenderJID)
			if reject != nil {
				return "", "", reject
			}
			if a.Emoji == "" {
				return target, "clear reaction on message " + a.MessageID, nil
			}
			return target, "react " + a.Emoji + " to message " + a.MessageID, nil
		},
		execute: func(ctx context.Context, a sendReactionArgs) (*mcp.CallToolResult, error) {
			if err := s.client.SendReaction(ctx, a.ChatJID, a.MessageID, a.SenderJID, a.Emoji); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText("Reaction sent"), nil
		},
	})
}

// -- send_reply -------------------------------------------------------------

type sendReplyArgs struct {
	ChatJID         string `json:"chat_jid"`
	TargetMessageID string `json:"target_message_id"`
	TargetSenderJID string `json:"target_sender_jid,omitempty"`
	Body            string `json:"body"`
}

func (s *Server) registerSendReply() {
	tool := mcp.NewTool("send_reply",
		mcp.WithDescription(queuedDesc("Send a text message that visibly quotes a previous message; recipients see the new text with the quoted message attached. Reversible via delete_message (revoke) or edit_message (correct text). Use send_message for a fresh non-quoting message and send_reaction for an emoji acknowledgement. Returns the plain-text string `Reply sent` on success.")),
		mcp.WithString("chat_jid", mcp.Required(), mcp.Description(jidDesc)),
		mcp.WithString("target_message_id", mcp.Required(), mcp.Description("WhatsApp message ID of the message being quoted (use `message_id` from list_messages)")),
		mcp.WithString("target_sender_jid", mcp.Description("JID of the quoted message's original sender; required in group chats, omit in 1:1 chats ("+jidDesc+")")),
		mcp.WithString("body", mcp.Required(), mcp.Description("reply text body")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
	registerGated(s, tool, gatedTool[sendReplyArgs]{
		prepare: func(_ context.Context, a sendReplyArgs) (string, string, *mcp.CallToolResult) {
			target, reject := prepareMessageRef(a.ChatJID, a.TargetMessageID, a.TargetSenderJID)
			if reject != nil {
				return "", "", reject
			}
			if r := requireNonEmpty("body", a.Body); r != nil {
				return "", "", r
			}
			return target, "reply to message " + a.TargetMessageID + " with " + previewText(a.Body), nil
		},
		execute: func(ctx context.Context, a sendReplyArgs) (*mcp.CallToolResult, error) {
			if err := s.client.SendReply(ctx, a.ChatJID, a.TargetMessageID, a.TargetSenderJID, a.Body); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText("Reply sent"), nil
		},
	})
}

// -- send_typing ------------------------------------------------------------

type sendTypingArgs struct {
	ChatJID string `json:"chat_jid"`
	Active  bool   `json:"active"`
	Kind    string `json:"kind,omitempty"`
}

func (s *Server) registerSendTyping() {
	tool := mcp.NewTool("send_typing",
		mcp.WithDescription("Show or hide the per-chat typing or recording presence indicator (only in chats on the operator's read allowlist, managed at /access); the recipient sees a transient `typing...` or `recording audio...` hint that auto-expires after roughly 25 seconds. Reversible by calling again with `active=false`. Use send_presence to set global online/offline availability instead. Returns the plain-text string `Presence active for <chat_jid>` or `Presence paused for <chat_jid>`."),
		mcp.WithString("chat_jid", mcp.Required(), mcp.Description(jidDesc)),
		mcp.WithBoolean("active", mcp.Required(), mcp.Description("true to show the indicator (composing or recording), false to pause it")),
		mcp.WithString("kind", mcp.Enum("", "audio"), mcp.Description("indicator kind: empty string for text typing (default) or `audio` for voice-note recording")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
	)
	s.mcp.AddTool(tool, mcp.NewTypedToolHandler(func(ctx context.Context, _ mcp.CallToolRequest, a sendTypingArgs) (*mcp.CallToolResult, error) {
		if r := s.requireReadable(ctx, "chat_jid", a.ChatJID); r != nil {
			return r, nil
		}
		if err := s.client.SendTyping(ctx, a.ChatJID, a.Active, a.Kind); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		state := "paused"
		if a.Active {
			state = "active"
		}
		return mcp.NewToolResultText(fmt.Sprintf("Presence %s for %s", state, a.ChatJID)), nil
	}))
}

// -- shared validation for gated send tools ---------------------------------

// prepareRecipient validates a send target and returns its canonical chat JID.
// markChatRead is the tool's mark_chat_read flag, which acks the chat and so
// needs read access to it.
func (s *Server) prepareRecipient(ctx context.Context, recipient string, markChatRead bool) (string, *mcp.CallToolResult) {
	if recipient == "" {
		return "", mcp.NewToolResultError("recipient must be provided")
	}
	target, err := chatJIDArg("recipient", recipient)
	if err != nil {
		return "", mcp.NewToolResultError(err.Error())
	}
	if r := s.requireMarkChatReadAllowed(ctx, markChatRead, target); r != nil {
		return "", r
	}
	return target, nil
}

// prepareMessageRef validates the chat, message ID and optional sender JID of
// a tool that acts on an existing message, and returns the canonical chat JID.
func prepareMessageRef(chatJID, messageID, senderJID string) (string, *mcp.CallToolResult) {
	if r := requireNonEmpty("chat_jid", chatJID); r != nil {
		return "", r
	}
	if r := requireNonEmpty("message_id", messageID); r != nil {
		return "", r
	}
	target, err := chatJIDArg("chat_jid", chatJID)
	if err != nil {
		return "", mcp.NewToolResultError(err.Error())
	}
	if senderJID != "" {
		if _, err := types.ParseJID(senderJID); err != nil {
			return "", mcp.NewToolResultError(fmt.Sprintf("sender_jid: invalid JID: %v", err))
		}
	}
	return target, nil
}
