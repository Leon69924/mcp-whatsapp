package main

import (
	"os"
	"path/filepath"
	"runtime"
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

func TestResolveApprovalConfig_ClientConfirmIsOptIn(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "0": false, "1": true} {
		cfg, err := resolveApprovalConfig("http://x", envOf(map[string]string{"WHATSAPP_MCP_CLIENT_CONFIRM": raw}))
		if err != nil || cfg.ClientConfirm != want {
			t.Fatalf("WHATSAPP_MCP_CLIENT_CONFIRM=%q: ClientConfirm = %v, %v; want %v", raw, cfg.ClientConfirm, err, want)
		}
	}
	if _, err := resolveApprovalConfig("http://x", envOf(map[string]string{"WHATSAPP_MCP_CLIENT_CONFIRM": "true"})); err == nil {
		t.Fatal("WHATSAPP_MCP_CLIENT_CONFIRM=true: want an error, not a silent off")
	}
}

func TestResolveAuthToken(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := write("token", "s3cret\n", 0o600)

	if got, err := resolveAuthToken(envOf(nil)); err != nil || got != "" {
		t.Fatalf("unset: %q, %v; want no token", got, err)
	}
	if got, err := resolveAuthToken(envOf(map[string]string{"WHATSAPP_MCP_TOKEN": "inline"})); err != nil || got != "inline" {
		t.Fatalf("inline: %q, %v", got, err)
	}
	if got, err := resolveAuthToken(envOf(map[string]string{"WHATSAPP_MCP_TOKEN_FILE": good})); err != nil || got != "s3cret" {
		t.Fatalf("file: %q, %v; want the trimmed token", got, err)
	}
	refused := map[string]map[string]string{
		"both":    {"WHATSAPP_MCP_TOKEN": "inline", "WHATSAPP_MCP_TOKEN_FILE": good},
		"missing": {"WHATSAPP_MCP_TOKEN_FILE": filepath.Join(dir, "nope")},
		"empty":   {"WHATSAPP_MCP_TOKEN_FILE": write("empty", "\n", 0o600)},
	}
	if runtime.GOOS != "windows" {
		refused["group-readable"] = map[string]string{"WHATSAPP_MCP_TOKEN_FILE": write("loose", "s3cret", 0o640)}
	}
	for name, env := range refused {
		if got, err := resolveAuthToken(envOf(env)); err == nil {
			t.Errorf("%s: token %q; want an error", name, got)
		}
	}
}

// Confirmation in the client without a token on /mcp would let any local
// process confirm its own actions, so serve refuses to start.
func TestRunServe_ClientConfirmRequiresToken(t *testing.T) {
	t.Setenv("WHATSAPP_MCP_ADDR", "127.0.0.1:0")
	t.Setenv("WHATSAPP_MCP_CLIENT_CONFIRM", "1")
	t.Setenv("WHATSAPP_MCP_TOKEN", "")
	t.Setenv("WHATSAPP_MCP_TOKEN_FILE", "")
	if code := runServe(t.TempDir(), nil, nil); code != 2 {
		t.Fatalf("runServe exit code = %d; want 2", code)
	}
}
