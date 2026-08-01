package audit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestLogger_RedactsSensitiveAttributes(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf)
	log.Info("event", map[string]any{
		"op":              "minted",
		"token":           "ghs_supersecret",
		"jwt":             "eyJhbGciOi...",
		"private_key_pem": "-----BEGIN...",
		"authorization":   "Bearer ghs_...",
		"repo":            "simons-agent-space/agentctl",
	})

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}
	for _, key := range []string{"token", "jwt", "private_key_pem", "authorization"} {
		if got := out[key]; got != "[REDACTED]" {
			t.Errorf("%s not redacted: %v", key, got)
		}
	}
	if got := out["repo"]; got != "simons-agent-space/agentctl" {
		t.Errorf("repo should pass through, got: %v", got)
	}
}

func TestLogger_DoesNotLeakTokenValue(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf)
	secret := "ghs_supersecret"
	log.Info("minted", map[string]any{
		"op":    "minted",
		"token": secret,
	})
	if strings.Contains(buf.String(), secret) {
		t.Errorf("token value leaked into log: %s", buf.String())
	}
}

func TestLogger_ProducesValidJSON(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf)
	log.Info("event", map[string]any{"op": "validated"})
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		var out map[string]any
		if err := json.Unmarshal([]byte(line), &out); err != nil {
			t.Errorf("line not valid JSON: %q: %v", line, err)
		}
		if out["level"] != "INFO" {
			t.Errorf("level = %v, want INFO", out["level"])
		}
	}
}

func TestRedactString(t *testing.T) {
	if got := RedactString("ghs_supersecret"); got != "[REDACTED]" {
		t.Errorf("RedactString() = %q, want [REDACTED]", got)
	}
	if got := RedactString(""); got != "" {
		t.Errorf("RedactString(\"\") = %q, want empty", got)
	}
}

func TestEvent_EmitInfo(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf)
	ev := &Event{Op: "validated", Repo: "simons-agent-space/agentctl", Profile: "builder"}
	ev.Emit(log)
	if !strings.Contains(buf.String(), `"op":"validated"`) {
		t.Errorf("op missing from log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"repo":"simons-agent-space/agentctl"`) {
		t.Errorf("repo missing from log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"profile":"builder"`) {
		t.Errorf("profile missing from log: %s", buf.String())
	}
}

func TestEvent_EmitRejectionWithError(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf)
	ev := &Event{Op: "rejected", Reason: "REPO_NOT_ALLOWED: nope"}
	ev.Emit(log)
	if !strings.Contains(buf.String(), `"level":"WARN"`) {
		t.Errorf("expected WARN level for rejection: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"reason":"REPO_NOT_ALLOWED: nope"`) {
		t.Errorf("reason missing from log: %s", buf.String())
	}
}
