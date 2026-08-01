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
		"token":           "***",
		"jwt":             "eyJhbGciOi...",
		"private_key_pem": "-----BEGIN...",
		"authorization":   "***",
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
	secret := "***"
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

func TestLogger_LevelsForEventKinds(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf)
	log.Info("validated", map[string]any{"op": "validated"})
	log.Info("minted", map[string]any{"op": "minted"})
	log.Warn("rejected", map[string]any{"op": "rejected", "reason": "nope"})
	if !strings.Contains(buf.String(), `"level":"INFO","msg":"validated"`) {
		t.Errorf("validated should be INFO: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"level":"INFO","msg":"minted"`) {
		t.Errorf("minted should be INFO: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"level":"WARN","msg":"rejected"`) {
		t.Errorf("rejected should be WARN: %s", buf.String())
	}
}

func TestRedactString(t *testing.T) {
	if got := RedactString("***"); got != "[REDACTED]" {
		t.Errorf("RedactString() = %q, want [REDACTED]", got)
	}
	if got := RedactString(""); got != "" {
		t.Errorf("RedactString(\"\") = %q, want empty", got)
	}
}
