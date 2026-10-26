package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sync"
)

// csrfGuard holds the single-use token a page embeds in its forms. Rendering a
// page mints a fresh token (replacing the previous one); a POST must present it
// and consumes it.
type csrfGuard struct {
	mu    sync.Mutex
	token string
}

// generate creates a new random token, stores it and returns it.
func (g *csrfGuard) generate() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback should never be reached in practice.
		panic(fmt.Sprintf("crypto/rand.Read failed: %v", err))
	}
	token := hex.EncodeToString(b)
	g.mu.Lock()
	g.token = token
	g.mu.Unlock()
	return token
}

// valid checks the supplied token against the stored one. A match consumes the
// token (single-use).
func (g *csrfGuard) valid(token string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token == "" || token == "" {
		return false
	}
	ok := g.token == token
	if ok {
		g.token = "" // single-use: consume after validation
	}
	return ok
}

// sameOriginPost reports whether a state-changing request was made by a page
// served from this daemon. Both browser provenance headers are required: the
// request must carry Sec-Fetch-Site: same-origin and an Origin naming the host
// it was addressed to. Requests without them (curl, scripts) are refused, so
// an operator decision has to come from the rendered page in a browser.
func sameOriginPost(r *http.Request) error {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "same-origin" {
		return fmt.Errorf("request refused: Sec-Fetch-Site must be same-origin (got %q)", site)
	}
	origin := r.Header.Get("Origin")
	u, err := url.Parse(origin)
	if origin == "" || err != nil || u.Host != r.Host {
		return fmt.Errorf("request refused: Origin must match the daemon host (got %q)", origin)
	}
	return nil
}
