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
// served from this daemon: a browser that marks the request cross-site
// (Sec-Fetch-Site other than same-origin), or sends an Origin that is not the
// host the request was addressed to, is refused. Clients that send neither
// header (curl) pass; the CSRF token still applies to them.
func sameOriginPost(r *http.Request) error {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return fmt.Errorf("cross-site request refused (Sec-Fetch-Site: %s)", site)
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			return fmt.Errorf("cross-origin request refused (Origin: %s)", origin)
		}
	}
	return nil
}
