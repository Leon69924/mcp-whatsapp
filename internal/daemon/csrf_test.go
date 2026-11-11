package daemon

import (
	"testing"
	"time"
)

// fakeClock is a settable clock for csrfGuard.now.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTestGuard() (*csrfGuard, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	return &csrfGuard{now: c.now}, c
}

func TestCSRFGuard_SeveralRendersStayValid(t *testing.T) {
	g, _ := newTestGuard()
	first, second := g.generate(), g.generate()
	if first == second {
		t.Fatal("two renders minted the same token")
	}
	if !g.valid(first) {
		t.Error("first token invalidated by a later render")
	}
	if !g.valid(second) {
		t.Error("second token refused")
	}
}

func TestCSRFGuard_TokenIsSingleUse(t *testing.T) {
	g, _ := newTestGuard()
	token := g.generate()
	if !g.valid(token) {
		t.Fatal("fresh token refused")
	}
	if g.valid(token) {
		t.Error("token accepted twice")
	}
}

func TestCSRFGuard_ExpiredTokenRefused(t *testing.T) {
	g, c := newTestGuard()
	token := g.generate()
	c.t = c.t.Add(csrfTokenTTL + time.Second)
	if g.valid(token) {
		t.Error("expired token accepted")
	}

	token = g.generate()
	c.t = c.t.Add(csrfTokenTTL)
	if !g.valid(token) {
		t.Error("token refused exactly at the TTL")
	}
}

func TestCSRFGuard_EmptyAndUnknownRefused(t *testing.T) {
	g, _ := newTestGuard()
	if g.valid("") {
		t.Error("empty token accepted on an empty guard")
	}
	g.generate()
	if g.valid("") {
		t.Error("empty token accepted")
	}
	if g.valid("not-issued") {
		t.Error("unknown token accepted")
	}
}

func TestCSRFGuard_ZeroValueUsable(t *testing.T) {
	var g csrfGuard
	if !g.valid(g.generate()) {
		t.Error("zero-value guard refused its own token")
	}
}

func TestCSRFGuard_CapEvictsOldest(t *testing.T) {
	g, c := newTestGuard()
	tokens := make([]string, 0, csrfMaxTokens+1)
	for i := 0; i <= csrfMaxTokens; i++ {
		tokens = append(tokens, g.generate())
		c.t = c.t.Add(time.Millisecond)
	}
	if g.valid(tokens[0]) {
		t.Error("oldest token survived exceeding the cap")
	}
	for _, token := range tokens[1:] {
		if !g.valid(token) {
			t.Fatal("a token within the cap was evicted")
		}
	}
}

func TestCSRFGuard_GeneratePrunesExpired(t *testing.T) {
	g, c := newTestGuard()
	g.generate()
	c.t = c.t.Add(csrfTokenTTL + time.Second)
	g.generate()
	if len(g.tokens) != 1 {
		t.Errorf("tokens = %d; want the expired one pruned", len(g.tokens))
	}
}
