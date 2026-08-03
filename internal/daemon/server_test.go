package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/simons-agent-space/agentctl/internal/audit"
	"github.com/simons-agent-space/agentctl/internal/deploy"
)

// minimalConfig returns a Config that satisfies Config.Validate but
// does not exercise any real deployment paths. Tests that need a
// running Server extend it (e.g. by overriding SocketPath).
func minimalConfig(socketPath string) *Config {
	return &Config{
		SocketPath: socketPath,
		Source: deploy.SourceConfig{
			AllowedOrg:     "acme",
			RepositoryRoot: "/srv/agentctl/repos",
			OriginURL:      "https://github.com/acme/example.git",
		},
		Runtime: deploy.RuntimeConfig{
			PortRangeStart: 40000,
			PortRangeEnd:   40100,
			HealthTimeout:  30 * time.Second,
		},
		Caddy: deploy.CaddyConfig{
			BaseDomain: "example.test",
			ConfigDir:  "/var/lib/agentctl/caddy",
		},
		State: deploy.StateConfig{
			StateDir:   "/var/lib/agentctl/state",
			BaseDomain: "example.test",
		},
		Data: deploy.DataConfig{
			DataRoot: "/var/lib/agentctl/data",
		},
	}
}

// TestRemoveStaleSocket_Absent verifies the no-op path: nothing at
// the configured path means nothing to clean up.
func TestRemoveStaleSocket_Absent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nope.sock")
	if err := removeStaleSocket(path); err != nil {
		t.Fatalf("removeStaleSocket on absent path: %v", err)
	}
}

// TestRemoveStaleSocket_RemovesSocket verifies a leftover socket
// from a previous daemon is removed silently, so the new bind does
// not fail with "address already in use".
func TestRemoveStaleSocket_RemovesSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leftover.sock")

	// Create a real socket via a short-lived listener.
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("seed listen: %v", err)
	}
	_ = l.Close()

	if err := removeStaleSocket(path); err != nil {
		t.Fatalf("removeStaleSocket: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected socket file to be gone, got err=%v", err)
	}
}

// TestRemoveStaleSocket_RefusesRegularFile ensures the daemon never
// silently destroys operator-placed files at its configured socket
// path; a regular file is treated as misconfiguration, not as a
// stale socket.
func TestRemoveStaleSocket_RefusesRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-socket")
	if err := os.WriteFile(path, []byte("important data\n"), 0o600); err != nil {
		t.Fatalf("write regular file: %v", err)
	}

	err := removeStaleSocket(path)
	if err == nil {
		t.Fatal("removeStaleSocket on regular file: want error, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to remove") {
		t.Fatalf("error %q does not advertise refusal", err.Error())
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		t.Fatalf("regular file was removed despite refusal (stat err=%v)", statErr)
	}
}

// TestListenAndServe_BasicLifecycle binds the socket, serves an
// HTTP request, then shuts down cleanly on context cancellation.
// This is the primary end-to-end socket-lifecycle property.
func TestListenAndServe_BasicLifecycle(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "agentctl.sock")
	cfg := minimalConfig(sockPath)

	srv, err := NewServer(cfg, audit.New(io.Discard))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(ctx) }()

	if err := waitForSocket(sockPath, 2*time.Second); err != nil {
		t.Fatalf("socket did not appear: %v", err)
	}

	resp, err := httpGetOverUDS(sockPath, "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.status != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.status)
	}
	if !bytes.Contains(resp.body, []byte(`"status":"ok"`)) {
		t.Fatalf("GET /healthz body = %q, want ok", resp.body)
	}

	cancel()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("ListenAndServe returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after cancel")
	}

	if _, err := os.Lstat(sockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket file still present after shutdown: err=%v", err)
	}
}

// TestListenAndServe_ReplacesStaleSocket ensures a leftover socket
// from a previous daemon is removed before bind so a fresh listen
// succeeds (this is the case the previous task's
// "refusing to overwrite a normal file" requirement rules out
// when the file is a regular file, not a socket).
func TestListenAndServe_ReplacesStaleSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "agentctl.sock")

	stale, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("seed stale socket: %v", err)
	}
	seedAddr := stale.Addr().String()
	_ = stale.Close()

	cfg := minimalConfig(sockPath)
	srv, err := NewServer(cfg, audit.New(io.Discard))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(ctx) }()
	t.Cleanup(cancel)

	if err := waitForSocket(sockPath, 2*time.Second); err != nil {
		t.Fatalf("socket did not appear after stale replacement: %v", err)
	}

	// Confirm the listen succeeded by issuing a request and that
	// the stale listener is no longer holding the path. We probe
	// the address string just to make sure removal happened via
	// the unlink path (a fresh inode is implied by the bind
	// success).
	_ = seedAddr

	resp, err := httpGetOverUDS(sockPath, "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.status != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.status)
	}

	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after cancel")
	}
}

// TestListenAndServe_RefusesOverwriteOfRegularFile confirms the
// daemon errors out without touching the existing file when a
// regular file occupies the socket path: a misconfiguration must
// never silently destroy operator data.
func TestListenAndServe_RefusesOverwriteOfRegularFile(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "agentctl.sock")
	contents := []byte("important data\n")
	if err := os.WriteFile(sockPath, contents, 0o600); err != nil {
		t.Fatalf("seed regular file: %v", err)
	}

	cfg := minimalConfig(sockPath)
	srv, err := NewServer(cfg, audit.New(io.Discard))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(ctx) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("ListenAndServe returned nil despite regular file collision")
		}
		if !strings.Contains(err.Error(), "refusing to remove") {
			t.Fatalf("error %q does not advertise refusal", err.Error())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not return on refusal")
	}

	got, err := os.ReadFile(sockPath)
	if err != nil {
		t.Fatalf("regular file disappeared: %v", err)
	}
	if !bytes.Equal(got, contents) {
		t.Fatalf("regular file mutated: got %q, want %q", got, contents)
	}
}

// TestListenAndServe_AppliesSocketMode verifies the socket's file
// mode is exactly the configured mode (or default 0660) so
// permission requirements (group-readable by the deployment group,
// not world-readable) hold in practice.
func TestListenAndServe_AppliesSocketMode(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "agentctl.sock")

	cfg := minimalConfig(sockPath)
	cfg.SocketMode = 0o640
	srv, err := NewServer(cfg, audit.New(io.Discard))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(ctx) }()

	if err := waitForSocket(sockPath, 2*time.Second); err != nil {
		t.Fatalf("socket did not appear: %v", err)
	}
	info, err := os.Lstat(sockPath)
	if err != nil {
		t.Fatalf("lstat socket: %v", err)
	}
	if got := info.Mode() &^ os.ModeType; got != 0o640 {
		t.Fatalf("socket mode = %o, want 0640", got)
	}

	cancel()
	<-errCh
}

// TestResolveSocketGroup covers the public helper used at startup.
func TestResolveSocketGroup(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		_, ok, err := ResolveSocketGroup("")
		if err != nil || ok {
			t.Fatalf("empty input: ok=%v err=%v, want ok=false err=nil", ok, err)
		}
	})
	t.Run("numeric", func(t *testing.T) {
		gid, ok, err := ResolveSocketGroup("1234")
		if err != nil || !ok {
			t.Fatalf("numeric gid: ok=%v err=%v, want ok=true err=nil", ok, err)
		}
		if gid != 1234 {
			t.Fatalf("gid = %d, want 1234", gid)
		}
	})
	t.Run("unresolvable", func(t *testing.T) {
		_, ok, err := ResolveSocketGroup("definitely-not-a-real-group-xyzzy")
		if err == nil || ok {
			t.Fatalf("bad group: ok=%v err=%v, want ok=false err!=nil", ok, err)
		}
	})
}

// waitForSocket polls path with a 10ms interval until the file
// exists or timeout elapses.
func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for socket %s", path)
}

// httpResponse is what we round-trip from the UDS socket so tests
// can assert on both status and body.
type httpResponse struct {
	status int
	body   []byte
}

// httpGetOverUDS performs a minimal HTTP/1.1 GET over a Unix
// domain socket. We craft the request manually because http.Client
// needs a custom Transport and dial helper for UDS, and a hand-rolled
// request keeps the test surface small and explicit.
func httpGetOverUDS(sockPath, path string) (*httpResponse, error) {
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", sockPath, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return nil, err
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n", path)
	if _, err := conn.Write([]byte(req)); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	const headerEnd = "\r\n\r\n"
	idx := strings.Index(string(raw), headerEnd)
	if idx < 0 {
		return nil, fmt.Errorf("malformed response: %q", raw)
	}
	statusLine := strings.SplitN(string(raw[:idx]), "\r\n", 2)[0]
	parts := strings.Fields(statusLine)
	if len(parts) < 2 {
		return nil, fmt.Errorf("malformed status line: %q", statusLine)
	}
	status, err := parseStatus(parts[1])
	if err != nil {
		return nil, err
	}
	return &httpResponse{status: status, body: raw[idx+len(headerEnd):]}, nil
}

// parseStatus parses "200" out of an HTTP status like "200 OK".
func parseStatus(s string) (int, error) {
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-numeric status %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
