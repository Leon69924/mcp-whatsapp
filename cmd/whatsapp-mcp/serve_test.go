package main

import (
	"strings"
	"testing"
	"time"
)

func TestIsLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8765": true,
		"127.0.0.1:0":    true,
		"[::1]:8765":     true,
		"localhost:8765": true,
		"0.0.0.0:8765":   false,
		"192.168.1.1:80": false,
		"10.0.0.1:8765":  false,
	}
	for addr, want := range cases {
		t.Run(addr, func(t *testing.T) {
			if got := isLoopbackAddr(addr); got != want {
				t.Fatalf("isLoopbackAddr(%q): want %v, got %v", addr, want, got)
			}
		})
	}
}

func envOf(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestResolveApprovalConfig(t *testing.T) {
	cfg, err := resolveApprovalConfig("http://127.0.0.1:8765", envOf(nil))
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if cfg.TTL != 24*time.Hour || cfg.Notify || cfg.BaseURL != "http://127.0.0.1:8765" {
		t.Fatalf("defaults = %+v", cfg)
	}

	cfg, err = resolveApprovalConfig("http://x", envOf(map[string]string{
		"WHATSAPP_MCP_APPROVAL_TTL": "90m",
		"WHATSAPP_MCP_NOTIFY":       "1",
	}))
	if err != nil || cfg.TTL != 90*time.Minute || !cfg.Notify {
		t.Fatalf("configured = %+v, %v", cfg, err)
	}

	if cfg, _ = resolveApprovalConfig("http://x", envOf(map[string]string{"WHATSAPP_MCP_NOTIFY": "true"})); cfg.Notify {
		t.Fatal("only WHATSAPP_MCP_NOTIFY=1 enables notifications")
	}
}

func TestResolveApprovalConfig_RejectsBadTTL(t *testing.T) {
	for _, raw := range []string{"tomorrow", "24", "0s", "-1h"} {
		_, err := resolveApprovalConfig("http://x", envOf(map[string]string{"WHATSAPP_MCP_APPROVAL_TTL": raw}))
		if err == nil || !strings.Contains(err.Error(), "WHATSAPP_MCP_APPROVAL_TTL") {
			t.Fatalf("TTL %q: error = %v; want one naming the variable", raw, err)
		}
	}
}
