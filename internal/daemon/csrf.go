package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	// csrfTokenTTL bounds how long a rendered form stays submittable, so an
	// abandoned tab cannot be replayed indefinitely.
	csrfTokenTTL = 15 * time.Minute
	// csrfMaxTokens caps the live set so repeated page loads cannot grow it
	// without bound; the oldest token is evicted first.
	csrfMaxTokens = 64
)

// csrfGuard holds the single-use tokens a page embeds in its forms. Every
// render mints its own token and adds it to the set, so several open tabs,
// reloads or prefetches do not invalidate each other. A POST must present a
// token that is still within csrfTokenTTL and consumes it. The zero value is
// ready to use.
type csrfGuard struct {
	mu     sync.Mutex
	tokens map[string]time.Time // token -> issue time
	now    func() time.Time     // clock; nil means time.Now (tests inject one)
}

func (g *csrfGuard) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// generate mints a new random token, adds it to the set and returns it. Expired
// tokens are pruned, and the oldest is evicted once the set exceeds
// csrfMaxTokens.
func (g *csrfGuard) generate() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback should never be reached in practice.
		panic(fmt.Sprintf("crypto/rand.Read failed: %v", err))
	}
	token := hex.EncodeToString(b)
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	if g.tokens == nil {
		g.tokens = make(map[string]time.Time)
	}
	for t, issued := range g.tokens {
		if now.Sub(issued) > csrfTokenTTL {
			delete(g.tokens, t)
		}
	}
	g.tokens[token] = now
	for len(g.tokens) > csrfMaxTokens {
		var oldest string
		var oldestAt time.Time
		for t, issued := range g.tokens {
			if oldest == "" || issued.Before(oldestAt) {
				oldest, oldestAt = t, issued
			}
		}
		delete(g.tokens, oldest)
	}
	return token
}

// valid reports whether token was issued by this guard and has not expired. A
// match consumes the token (single-use); an expired one is dropped.
func (g *csrfGuard) valid(token string) bool {
	if token == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	issued, ok := g.tokens[token]
	if !ok {
		return false
	}
	delete(g.tokens, token) // single-use: consume after lookup
	return g.clock().Sub(issued) <= csrfTokenTTL
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
