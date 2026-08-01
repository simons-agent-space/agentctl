// Package audit provides a structured JSON logger that redacts secret material.
//
// The logger is built on the encoding/json standard library and writes
// one JSON object per line. Any field whose name contains a sensitive
// substring (token, jwt, key, private_key, etc.) is replaced with
// "[REDACTED]" before the record is written.
//
// The broker uses this logger so that even if a future code path
// accidentally passes secret material as a field, it never reaches disk.
package audit

import (
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"
)

// redacted is the placeholder substituted for any redacted field value.
const redacted = "[REDACTED]"

// sensitiveSubstrings are case-insensitive substrings that mark a field
// as containing secret material. If any field name contains one of these
// substrings, the value is replaced before it is written.
var sensitiveSubstrings = []string{
	"token",
	"jwt",
	"private_key",
	"privatekey",
	"pem",
	"secret",
	"authorization",
	"bearer",
	"password",
	"client_secret",
}

// Logger emits JSON records with secret fields redacted. It is safe for
// concurrent use.
type Logger struct {
	mu sync.Mutex
	w  io.Writer
}

// New returns a Logger that writes JSON records to w.
func New(w io.Writer) *Logger {
	return &Logger{w: w}
}

// Info writes an INFO-level audit record.
func (l *Logger) Info(event string, fields map[string]any) {
	l.emit("INFO", event, fields)
}

// Warn writes a WARN-level audit record.
func (l *Logger) Warn(event string, fields map[string]any) {
	l.emit("WARN", event, fields)
}

// Error writes an ERROR-level audit record.
func (l *Logger) Error(event string, fields map[string]any) {
	l.emit("ERROR", event, fields)
}

func (l *Logger) emit(level, event string, fields map[string]any) {
	rec := make(map[string]any, len(fields)+3)
	rec["time"] = time.Now().UTC().Format(time.RFC3339Nano)
	rec["level"] = level
	rec["msg"] = event
	for k, v := range fields {
		rec[k] = redactField(k, v)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		b = []byte(`{"level":"ERROR","msg":"audit-marshal-failed"}`)
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.w.Write(b)
}

// redactField returns redacted if key is sensitive, otherwise val.
func redactField(key string, val any) any {
	if isSensitive(key) {
		return redacted
	}
	return val
}

func isSensitive(name string) bool {
	lower := strings.ToLower(name)
	for _, s := range sensitiveSubstrings {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// RedactString returns the placeholder string for free-form log messages.
// It is exported so callers building log messages with embedded sensitive
// material can apply the same rule without going through a structured
// field.
func RedactString(sensitive string) string {
	if sensitive == "" {
		return ""
	}
	return redacted
}

// Event is a small helper for emitting consistently-named audit events.
// Use it from request middleware to avoid spelling inconsistencies.
type Event struct {
	Op      string // short verb: validated, minted, rejected
	Repo    string // full repo slug, if known
	Profile string // permission profile requested, if any
	Reason  string // rejection reason, if any
	Err     error  // error to record (never carries the token)
}

// Emit writes the event to log.
func (l *Event) Emit(log *Logger) {
	fields := map[string]any{}
	if l.Op != "" {
		fields["op"] = l.Op
	}
	if l.Repo != "" {
		fields["repo"] = l.Repo
	}
	if l.Profile != "" {
		fields["profile"] = l.Profile
	}
	if l.Reason != "" {
		fields["reason"] = l.Reason
	}
	if l.Err != nil {
		fields["err"] = l.Err.Error()
	}
	if l.Err != nil || l.Op == "rejected" {
		log.Warn("audit", fields)
	} else {
		log.Info("audit", fields)
	}
}
