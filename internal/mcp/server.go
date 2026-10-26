// Package mcp wires mark3labs/mcp-go to the internal/client and internal/store
// packages, exposing the WhatsApp bridge over MCP Streamable HTTP.
package mcp

import (
	"net/http"

	"github.com/mark3labs/mcp-go/server"

	"github.com/sealjay/mcp-whatsapp/internal/client"
	"github.com/sealjay/mcp-whatsapp/internal/store"
)

// pairingCache is the minimal pairing-state surface pairing_status needs.
// *daemon.PairCache satisfies it. The smoke command and tests pass nil, in
// which case pairing_status reports an "error" envelope.
type pairingCache interface {
	Paired() bool
	QR() string
}

// Server holds the MCP server, its bound WhatsApp client, and an optional
// pairing cache (used only by pairing_status).
type Server struct {
	client *client.Client
	mcp    *server.MCPServer
	cache  pairingCache
	// store is the client's message cache; nil only when the server is built
	// without a client (smoke and unit tests).
	store     *store.Store
	approvals ApprovalConfig
	// executors maps each approval-gated tool to the handler that runs it once
	// the operator approves a queued call.
	executors map[string]executor
}

// NewServer constructs an MCP server with all tools registered against the
// provided WhatsApp client. cache may be nil (smoke/tests); pairing_status is
// the only tool that consults it.
// Version is reported as serverInfo.version. cmd/whatsapp-mcp sets it from its
// -ldflags build version so the two cannot drift.
var Version = "dev"

func NewServer(c *client.Client, cache pairingCache, opts ...Option) *Server {
	mcpSrv := server.NewMCPServer(
		"whatsapp",
		Version,
		server.WithToolCapabilities(true),
	)
	s := &Server{
		client:    c,
		mcp:       mcpSrv,
		cache:     cache,
		approvals: ApprovalConfig{TTL: DefaultApprovalTTL},
		executors: map[string]executor{},
	}
	if c != nil {
		s.store = c.Store()
	}
	for _, opt := range opts {
		opt(s)
	}
	s.registerTools()
	s.registerResources()
	return s
}

// MCP returns the underlying mcp-go server (tests only).
func (s *Server) MCP() *server.MCPServer { return s.mcp }

// AttachHTTP mounts the MCP Streamable HTTP handler on mux at /mcp. The
// actual listener lifecycle is owned by the caller (internal/daemon).
func (s *Server) AttachHTTP(mux *http.ServeMux) {
	// Stateless: nothing here is session-scoped (the pairing cache is process-wide
	// device state), so legacy clients get no Mcp-Session-Id either.
	httpHandler := server.NewStreamableHTTPServer(s.mcp, server.WithStateLess(true))
	mux.Handle("/mcp", httpHandler)
}
