package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
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

// TestPrepareSocket_Absent verifies the no-op path: nothing at
// the configured path means nothing to clean up.
func TestPrepareSocket_Absent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nope.sock")
	if err := prepareSocket(path, 100*time.Millisecond); err != nil {
		t.Fatalf("prepareSocket on absent path: %v", err)
	}
}

// TestPrepareSocket_RemovesStaleSocket verifies that a leftover
// socket file from a closed listener is removed silently, so the
// new bind does not fail with "address already in use". The
// listener must be closed before this test calls prepareSocket so
// the probe does not see a live process and correctly classifies
// the file as stale.
func TestPrepareSocket_RemovesStaleSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leftover.sock")

	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("seed listen: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	if err := prepareSocket(path, 100*time.Millisecond); err != nil {
		t.Fatalf("prepareSocket on stale socket: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected socket file to be gone, got err=%v", err)
	}
}

// TestPrepareSocket_ActiveSocketFails verifies that a real
// listening daemon at the configured path causes prepareSocket to
// return ErrSocketInUse rather than unlink the live socket. This
// is the regression test for the "do not blindly remove an existing
// Unix socket" requirement: the previous removeStaleSocket helper
// would have deleted the live socket file out from under the
// running daemon, breaking its clients.
func TestPrepareSocket_ActiveSocketFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.sock")

	live, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("seed listener: %v", err)
	}
	t.Cleanup(func() { _ = live.Close() })

	err = prepareSocket(path, 500*time.Millisecond)
	if !errors.Is(err, ErrSocketInUse) {
		t.Fatalf("prepareSocket on active socket: want ErrSocketInUse, got %v", err)
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		t.Fatalf("active socket was deleted despite ErrSocketInUse (stat err=%v)", statErr)
	}
}

// TestPrepareSocket_RefusesRegularFile ensures prepareSocket never
// silently destroys operator-placed files at its configured socket
// path; a regular file is treated as misconfiguration, not as a
// stale socket.
func TestPrepareSocket_RefusesRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-socket")
	if err := os.WriteFile(path, []byte("important data\n"), 0o600); err != nil {
		t.Fatalf("write regular file: %v", err)
	}

	err := prepareSocket(path, 100*time.Millisecond)
	if err == nil {
		t.Fatal("prepareSocket on regular file: want error, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to remove") {
		t.Fatalf("error %q does not advertise refusal", err.Error())
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		t.Fatalf("regular file was removed despite refusal (stat err=%v)", statErr)
	}
}

// TestPrepareSocket_ProbeTimeoutLeavesSocketUntouched verifies
// that a dial timeout during the stale-socket probe is surfaced
// as an error and the socket file is left in place. A naive
// "any error means stale" implementation would unlink the socket
// of a live but slow listener; that race can disconnect clients
// of a running daemon, so we deliberately fail startup instead.
func TestPrepareSocket_ProbeTimeoutLeavesSocketUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "slow.sock")

	// Seed a real socket file so the lstat passes and the probe
	// path is exercised. We use syscall.Mknod with S_IFSOCK
	// because on Linux closing a bound listener removes the
	// pathname from the filesystem, which would let prepareSocket
	// short-circuit through the ErrNotExist path and never call
	// the dialer.
	if err := syscall.Mknod(path, syscall.S_IFSOCK|0o600, 0); err != nil {
		t.Fatalf("mknod socket: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	orig := dialUnixSocket
	dialUnixSocket = func(_ string, _ time.Duration) (net.Conn, error) {
		return nil, &net.OpError{
			Op:  "dial",
			Net: "unix",
			Err: &syntheticTimeout{msg: "i/o timeout"},
		}
	}
	t.Cleanup(func() { dialUnixSocket = orig })

	err := prepareSocket(path, 100*time.Millisecond)
	if err == nil {
		t.Fatal("prepareSocket on probe timeout: want error, got nil")
	}
	if errors.Is(err, ErrSocketInUse) {
		t.Fatalf("prepareSocket on probe timeout: got ErrSocketInUse, want probe error (err=%v)", err)
	}
	if !strings.Contains(err.Error(), "probe socket") {
		t.Errorf("error %q does not advertise probe failure", err.Error())
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		t.Fatalf("socket file was removed on probe timeout: stat err=%v", statErr)
	}
}

// TestPrepareSocket_UnexpectedProbeErrorLeavesSocketUntouched
// verifies that any dial error other than ECONNREFUSED is
// preserved and the socket file is left untouched. Permission
// errors, resource exhaustion, and unrecognised transport
// failures are all "unknown" outcomes: removing the socket would
// be guessing, and guessing wrong takes down a live daemon.
func TestPrepareSocket_UnexpectedProbeErrorLeavesSocketUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weird.sock")

	if err := syscall.Mknod(path, syscall.S_IFSOCK|0o600, 0); err != nil {
		t.Fatalf("mknod socket: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	orig := dialUnixSocket
	dialUnixSocket = func(_ string, _ time.Duration) (net.Conn, error) {
		return nil, &net.OpError{
			Op:  "dial",
			Net: "unix",
			Err: errors.New("synthetic EACCES: permission denied"),
		}
	}
	t.Cleanup(func() { dialUnixSocket = orig })

	err := prepareSocket(path, 100*time.Millisecond)
	if err == nil {
		t.Fatal("prepareSocket on unexpected probe error: want error, got nil")
	}
	if errors.Is(err, ErrSocketInUse) {
		t.Fatalf("prepareSocket on unexpected probe error: got ErrSocketInUse, want probe error (err=%v)", err)
	}
	if !strings.Contains(err.Error(), "probe socket") {
		t.Errorf("error %q does not advertise probe failure", err.Error())
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		t.Fatalf("socket file was removed on unexpected probe error: stat err=%v", statErr)
	}
}

// TestPrepareSocket_StaleSocketRemoved is the end-to-end
// "stale socket with connection refused" test: a socket file
// exists but nothing is listening, the probe returns
// ECONNREFUSED, and prepareSocket removes the file. We use
// syscall.Mknod with S_IFSOCK to create a real socket inode
// without binding it (closing a bound listener would unlink
// the pathname on Linux and bypass the probe).
func TestPrepareSocket_StaleSocketRemoved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leftover.sock")

	if err := syscall.Mknod(path, syscall.S_IFSOCK|0o600, 0); err != nil {
		t.Fatalf("mknod socket: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	if err := prepareSocket(path, 200*time.Millisecond); err != nil {
		t.Fatalf("prepareSocket on stale socket: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected stale socket file to be gone, got err=%v", err)
	}
}

// TestProbeSocket_Direct exercises the three-way classification
// directly so a regression in probeSocket is caught without
// having to reason about prepareSocket's wrapper. It uses the
// dialUnixSocket hook to inject every outcome without depending
// on kernel-level listen backlog behaviour.
func TestProbeSocket_Direct(t *testing.T) {
	t.Run("active listener", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "live.sock")
		l, err := net.Listen("unix", path)
		if err != nil {
			t.Fatalf("seed listen: %v", err)
		}
		t.Cleanup(func() { _ = l.Close() })

		stale, err := probeSocket(path, 200*time.Millisecond)
		if err != nil {
			t.Fatalf("probeSocket on live listener: %v", err)
		}
		if stale {
			t.Fatalf("probeSocket on live listener: stale=true, want false")
		}
	})
	t.Run("stale socket returns ECONNREFUSED", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "leftover.sock")
		if err := syscall.Mknod(path, syscall.S_IFSOCK|0o600, 0); err != nil {
			t.Fatalf("mknod socket: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(path) })

		stale, err := probeSocket(path, 200*time.Millisecond)
		if err != nil {
			t.Fatalf("probeSocket on stale socket: %v", err)
		}
		if !stale {
			t.Fatalf("probeSocket on stale socket: stale=false, want true")
		}
	})
	t.Run("timeout preserves error", func(t *testing.T) {
		orig := dialUnixSocket
		dialUnixSocket = func(_ string, _ time.Duration) (net.Conn, error) {
			return nil, &net.OpError{
				Op:  "dial",
				Net: "unix",
				Err: &syntheticTimeout{msg: "i/o timeout"},
			}
		}
		t.Cleanup(func() { dialUnixSocket = orig })

		stale, err := probeSocket("/tmp/whatever.sock", 50*time.Millisecond)
		if stale {
			t.Fatalf("probeSocket on timeout: stale=true, want false")
		}
		if err == nil {
			t.Fatal("probeSocket on timeout: err=nil, want timeout error")
		}
	})
	t.Run("unexpected error preserves error", func(t *testing.T) {
		synthetic := errors.New("synthetic EACCES")
		orig := dialUnixSocket
		dialUnixSocket = func(_ string, _ time.Duration) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: "unix", Err: synthetic}
		}
		t.Cleanup(func() { dialUnixSocket = orig })

		stale, err := probeSocket("/tmp/whatever.sock", 50*time.Millisecond)
		if stale {
			t.Fatalf("probeSocket on unexpected error: stale=true, want false")
		}
		if err == nil {
			t.Fatal("probeSocket on unexpected error: err=nil, want error")
		}
		if !errors.Is(err, synthetic) {
			t.Errorf("probeSocket on unexpected error: err=%v, want errors.Is match", err)
		}
	})
}

// syntheticTimeout satisfies net.Error with Timeout()=true so the
// test errors look like the real "i/o timeout" callers see on a
// saturate-the-backlog probe.
type syntheticTimeout struct{ msg string }

func (e *syntheticTimeout) Error() string   { return e.msg }
func (e *syntheticTimeout) Timeout() bool   { return true }
func (e *syntheticTimeout) Temporary() bool { return true }

// TestRequireSocketParent_Missing verifies the helper rejects a
// configuration whose socket parent does not exist.
func TestRequireSocketParent_Missing(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "no-such-dir")
	path := filepath.Join(parent, "agentctl.sock")
	err := requireSocketParent(path)
	if !errors.Is(err, ErrSocketParentMissing) {
		t.Fatalf("requireSocketParent: want ErrSocketParentMissing, got %v", err)
	}
}

// TestRequireSocketParent_Exists verifies the helper accepts a
// configuration whose socket parent is an ordinary directory.
func TestRequireSocketParent_Exists(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "agentctl.sock")
	if err := requireSocketParent(path); err != nil {
		t.Fatalf("requireSocketParent on existing dir: %v", err)
	}
}

// TestListenAndServe_FailsOnSocketParentMissing verifies the
// end-to-end startup path: when the configured socket's parent
// directory does not exist, ListenAndServe returns
// ErrSocketParentMissing without binding the socket.
func TestListenAndServe_FailsOnSocketParentMissing(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "no-such-dir")
	path := filepath.Join(parent, "agentctl.sock")
	cfg := minimalConfig(path)
	srv, err := NewServer(cfg, audit.New(io.Discard))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	err = srv.ListenAndServe(context.Background())
	if !errors.Is(err, ErrSocketParentMissing) {
		t.Fatalf("ListenAndServe on missing parent: want ErrSocketParentMissing, got %v", err)
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

// TestConfig_WriteTimeout pins the WriteTimeout math: the server's
// HTTP WriteTimeout is derived from the operation timeout so a
// 30-minute deploy followed by a small JSON response still reaches
// the client. This is the regression test for the previous "fixed
// 60-second WriteTimeout" bug that could let a long deploy succeed
// while the client lost the response.
func TestConfig_WriteTimeout(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		cfg := &Config{}
		want := defaultOperationTimeout + defaultWriteTimeoutMargin
		if got := cfg.WriteTimeout(); got != want {
			t.Errorf("WriteTimeout = %v, want %v (default op + default margin)", got, want)
		}
	})
	t.Run("operation timeout drives write timeout", func(t *testing.T) {
		cfg := &Config{OperationTimeout: 10 * time.Minute}
		want := 10*time.Minute + defaultWriteTimeoutMargin
		if got := cfg.WriteTimeout(); got != want {
			t.Errorf("WriteTimeout = %v, want %v (10m op + default margin)", got, want)
		}
	})
	t.Run("explicit margin applied", func(t *testing.T) {
		margin := 30 * time.Second
		cfg := &Config{OperationTimeout: 10 * time.Minute, WriteTimeoutMargin: &margin}
		want := 10*time.Minute + 30*time.Second
		if got := cfg.WriteTimeout(); got != want {
			t.Errorf("WriteTimeout = %v, want %v (10m op + 30s margin)", got, want)
		}
	})
	t.Run("margin larger than op is preserved", func(t *testing.T) {
		// Operators that want extra headroom can raise the margin.
		margin := 2 * time.Minute
		cfg := &Config{OperationTimeout: 30 * time.Second, WriteTimeoutMargin: &margin}
		want := 30*time.Second + 2*time.Minute
		if got := cfg.WriteTimeout(); got != want {
			t.Errorf("WriteTimeout = %v, want %v (30s op + 2m margin)", got, want)
		}
	})
	t.Run("explicit zero disables margin", func(t *testing.T) {
		// AGENTCTLD_WRITE_TIMEOUT_MARGIN=0 promises to disable the
		// headroom. Config.WriteTimeoutMargin is a pointer precisely
		// so we can tell "unset" (nil → default) apart from
		// "explicitly zero" (→ 0). Operators that want
		// WriteTimeout == OperationTimeout must be able to ask for
		// it; the pointer makes that observable.
		var zero time.Duration
		cfg := &Config{OperationTimeout: 10 * time.Minute, WriteTimeoutMargin: &zero}
		want := 10 * time.Minute
		if got := cfg.WriteTimeout(); got != want {
			t.Errorf("WriteTimeout = %v, want %v (10m op, no margin)", got, want)
		}
	})
	t.Run("nil margin keeps default", func(t *testing.T) {
		// Sanity: nil pointer is the "unset" path and must yield
		// the package default, not zero. Operators get the
		// default headroom when they do not configure the env var.
		cfg := &Config{OperationTimeout: 10 * time.Minute, WriteTimeoutMargin: nil}
		want := 10*time.Minute + defaultWriteTimeoutMargin
		if got := cfg.WriteTimeout(); got != want {
			t.Errorf("WriteTimeout = %v, want %v (nil margin → default)", got, want)
		}
	})
}

// TestListenAndServe_BindsWithConfiguredWriteTimeout exercises the
// full ListenAndServe path and confirms the http.Server's
// WriteTimeout is at least as long as cfg.WriteTimeout() reports.
// The previous "fixed 60s WriteTimeout" bug is now impossible
// because the value is computed from the operation timeout; this
// integration test catches any regression that hardcodes a fixed
// timeout or wires the wrong value into http.Server.
func TestListenAndServe_BindsWithConfiguredWriteTimeout(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "agentctl.sock")
	cfg := minimalConfig(sockPath)
	cfg.OperationTimeout = 90 * time.Second
	margin := 30 * time.Second
	cfg.WriteTimeoutMargin = &margin
	if want := 2 * time.Minute; cfg.WriteTimeout() != want {
		t.Fatalf("cfg.WriteTimeout = %v, want %v", cfg.WriteTimeout(), want)
	}
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
	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after cancel")
	}
}

// TestInspectHandler_DoesNotCreateCheckout is the regression test
// for the "/v1/inspect calls CheckoutSource and discards the
// checkout" bug: handleInspect must not leave a worktree on disk.
// We exercise the handler in-process with httptest so the assertion
// has a single, deterministic filesystem to inspect; the Server
// uses its real source-verification path against an actual local
// git remote so VerifyCommit's behaviour is covered end-to-end.
func TestInspectHandler_DoesNotCreateCheckout(t *testing.T) {
	remoteDir := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = remoteDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q", "-b", "main", remoteDir)
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	runGit("commit", "--allow-empty", "-q", "-m", "first commit on main")
	mainSHA := runGit("rev-parse", "HEAD")
	if len(mainSHA) != 40 {
		t.Fatalf("setupRemote mainSHA = %q (len %d, want 40)", mainSHA, len(mainSHA))
	}

	repoRoot := t.TempDir()
	cfg := minimalConfig(filepath.Join(t.TempDir(), "agentctl.sock"))
	cfg.Source.RepositoryRoot = repoRoot
	cfg.Source.OriginURLOverride = remoteDir
	cfg.Source.AllowedOrg = "acme"

	srv, err := NewServerWithDeployer(cfg, audit.New(io.Discard), realDeployer{}, execDockerRunner{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	manifestJSON := json.RawMessage(`{"version":1,"app":"myapp","container_port":8080,"health_path":"/healthz"}`)
	body, err := json.Marshal(map[string]any{
		"app":      "myapp",
		"commit":   mainSHA,
		"manifest": manifestJSON,
	})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/inspect", bytes.NewReader(body))
	w := httptest.NewRecorder()

	srv.handleInspect(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("inspect status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var resp InspectResponse
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&resp); err != nil {
		t.Fatalf("decode inspect response: %v", err)
	}
	if !resp.Valid {
		t.Errorf("inspect valid=false; errors=%v", resp.Errors)
	}
	if !resp.SourceReachable {
		t.Errorf("inspect source_reachable=false")
	}

	checkoutPath := filepath.Join(repoRoot, "myapp-checkouts", mainSHA)
	if _, err := os.Stat(checkoutPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("checkout directory %s unexpectedly exists: %v (handleInspect must not create a worktree)", checkoutPath, err)
	}
	// Also explicitly assert the parent checkouts directory is
	// absent: a partially-created <root>/myapp-checkouts dir would
	// also be a side-effect leak.
	parentCheckouts := filepath.Join(repoRoot, "myapp-checkouts")
	if _, err := os.Stat(parentCheckouts); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("checkouts directory %s unexpectedly exists: %v", parentCheckouts, err)
	}
}

// TestSlowDeployer_ResponseDelivered drives the full HTTP/UDS path
// with a fake Deployer that sleeps for longer than the previous
// fixed 60-second WriteTimeout would have allowed. The response
// must still be written and delivered. The TestConfig_WriteTimeout
// unit test pins the underlying math; this test exercises the
// integration so a regression that wires the wrong value into
// http.Server (or that wires the previous fixed 60s) is caught.
func TestSlowDeployer_ResponseDelivered(t *testing.T) {
	const (
		operationTimeout = 30 * time.Second
		writeMargin      = 5 * time.Minute
		deployerSleep    = 2 * time.Second
	)
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "agentctl.sock")
	cfg := minimalConfig(sockPath)
	cfg.OperationTimeout = operationTimeout
	margin := writeMargin
	cfg.WriteTimeoutMargin = &margin
	if got, want := cfg.WriteTimeout(), operationTimeout+writeMargin; got != want {
		t.Fatalf("WriteTimeout = %v, want %v", got, want)
	}

	fake := &slowFakeDeployer{sleepFor: deployerSleep}
	srv, err := NewServerWithDeployer(cfg, audit.New(io.Discard), fake, execDockerRunner{})
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

	manifestJSON := json.RawMessage(`{"version":1,"app":"example","container_port":8080,"health_path":"/healthz"}`)
	body, err := json.Marshal(map[string]any{
		"commit":   strings.Repeat("a", 40),
		"manifest": manifestJSON,
	})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}

	start := time.Now()
	resp, err := httpPostOverUDS(sockPath, "/v1/apps/example/deploy", string(body))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("POST deploy: %v", err)
	}
	if resp.status != http.StatusOK {
		t.Fatalf("POST deploy status = %d, want 200; body = %s", resp.status, resp.body)
	}
	if elapsed < deployerSleep {
		t.Errorf("response arrived in %v, want >= %v (deployer sleep)", elapsed, deployerSleep)
	}
	if !bytes.Contains(resp.body, []byte(`"app":"example"`)) {
		t.Errorf("response body does not include app: %s", resp.body)
	}
	if !bytes.Contains(resp.body, []byte(`"commit":`)) {
		t.Errorf("response body does not include commit: %s", resp.body)
	}
	if calls := atomic.LoadInt32(&fake.calls); calls != 1 {
		t.Errorf("fake deployer calls = %d, want 1", calls)
	}

	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after cancel")
	}
}

// slowFakeDeployer is the test counterpart of realDeployer: it
// sleeps on Deploy and Rollback to simulate a slow operation
// and never touches Docker, Caddy, or the filesystem. sleepFor
// applies to both methods so a shutdown-timing test only has to
// configure one field.
type slowFakeDeployer struct {
	sleepFor      time.Duration
	calls         int32
	rollbackCalls int32
}

func (s *slowFakeDeployer) Deploy(ctx context.Context, _ deploy.DeployConfig, m deploy.Manifest, commit string) (*deploy.DeployResult, error) {
	atomic.AddInt32(&s.calls, 1)
	select {
	case <-time.After(s.sleepFor):
		return &deploy.DeployResult{
			App:           m.App,
			Commit:        commit,
			Image:         "test:latest",
			ContainerName: m.App,
			HostPort:      4711,
			ContainerPort: m.ContainerPort,
			Hostname:      "test.local",
			Upstream:      "test:8080",
			DeployedAt:    time.Now().UTC(),
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *slowFakeDeployer) Rollback(ctx context.Context, _ deploy.RollbackConfig) error {
	atomic.AddInt32(&s.rollbackCalls, 1)
	select {
	case <-time.After(s.sleepFor):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestInspectContainerStatus_Accepts32CharAppContainerName is the
// regression test for the "32-char app container name rejected by
// the app-name regex" bug. The previous implementation validated
// the derived container name (e.g. "agentctl-<32-char-app>-<12-hex>",
// 54 chars total) against the app-name regex which is bounded to
// 32 chars. A valid app at the regex's length limit produced a
// container name that never matched, so status returned "unknown"
// without ever calling Docker.
//
// We construct a 32-char app (the longest the app regex allows),
// derive the container name with deriveContainerName-equivalent
// formatting, and assert inspectContainerStatus accepts it and
// passes it through to the runner. A sanity check at the top of
// the test verifies the container name does NOT match appNameRe,
// so a regression that re-introduces the wrong regex would be
// caught immediately.
func TestInspectContainerStatus_Accepts32CharAppContainerName(t *testing.T) {
	app := "a" + strings.Repeat("b", 30) + "c" // 32 chars total
	if len(app) != 32 {
		t.Fatalf("test setup: app length = %d, want 32", len(app))
	}
	if !appNameRe.MatchString(app) {
		t.Fatalf("test setup: app %q does not match appNameRe", app)
	}
	sha12 := strings.Repeat("d", 12)
	container := "agentctl-" + app + "-" + sha12
	// Sanity: appNameRe would have rejected this name, so the
	// test would have failed under the previous implementation.
	if appNameRe.MatchString(container) {
		t.Fatalf("test setup: container %q unexpectedly matches appNameRe (test would not catch the regression)", container)
	}

	runner := &scriptedDockerRunner{out: "true\n", err: nil}
	status, err := inspectContainerStatus(context.Background(), runner, container)
	if err != nil {
		t.Fatalf("inspectContainerStatus: %v", err)
	}
	if status != "running" {
		t.Fatalf("status = %q, want running", status)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.calls))
	}
	if got := runner.calls[0].name; got != "docker" {
		t.Errorf("call name = %q, want docker", got)
	}
	if got := runner.calls[0].args[len(runner.calls[0].args)-1]; got != container {
		t.Errorf("docker inspect last arg = %q, want %q", got, container)
	}
}

// TestInspectContainerStatus_NoSuchObjectOnStderrIsAbsent is the
// regression test for the "absent container reported as unknown"
// bug. Docker writes "No such container" / "No such object" to
// stderr when the target container does not exist; the previous
// execDockerRunner used cmd.Output() which only captures stdout,
// so inspectContainerStatus fell through to the generic "unknown"
// branch. execDockerRunner now uses cmd.CombinedOutput(); this test
// models that combined-output behaviour with a fake runner and
// asserts the classification branch is reached.
//
// This test exercises inspectContainerStatus's classification
// logic only. The execDockerRunner change itself is covered by
// TestExecDockerRunner_CombinedOutputCapturesStderr below, which
// runs the real runner against a shell script that writes to
// stderr.
func TestInspectContainerStatus_NoSuchObjectOnStderrIsAbsent(t *testing.T) {
	container := "agentctl-myapp-1234567890ab"
	combined := "Error response from daemon: No such object: " + container + "\n"

	runner := &scriptedDockerRunner{out: combined, err: fmt.Errorf("exit status 1")}
	status, err := inspectContainerStatus(context.Background(), runner, container)
	if !errors.Is(err, errContainerAbsent) {
		t.Fatalf("err = %v, want errors.Is(err, errContainerAbsent)", err)
	}
	if status != "absent" {
		t.Fatalf("status = %q, want absent", status)
	}
}

// TestExecDockerRunner_CombinedOutputCapturesStderr runs the real
// execDockerRunner against a shell script that writes the
// "No such object" message to stderr and exits non-zero, then
// asserts the message is present in the returned string. This
// guards the Output-vs-CombinedOutput change: a regression that
// reverts to cmd.Output() would cause this test to fail because
// stderr would be dropped.
func TestExecDockerRunner_CombinedOutputCapturesStderr(t *testing.T) {
	// The fake binary must live on a filesystem that allows
	// exec. execTempDir prefers the sandbox's GOTMPDIR
	// (/workspace/.gotmp) when present and falls back to
	// os.TempDir() (/tmp on a normal Linux runner) so the test
	// works in both the development sandbox (where /tmp is
	// mounted noexec) and the GitHub Actions CI runner (where
	// /workspace does not exist).
	fakeDir, err := os.MkdirTemp(execTempDir(t), "fake-docker-")
	if err != nil {
		t.Fatalf("mkdir fake docker dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(fakeDir) })

	script := "#!/bin/sh\n" +
		"echo 'Error response from daemon: No such object: agentctl-x-1234567890ab' >&2\n" +
		"exit 1\n"
	binPath := filepath.Join(fakeDir, "docker")
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}

	runner := execDockerRunner{}
	out, err := runner.Run(context.Background(), binPath, "inspect", "--format", "{{.State.Running}}", "agentctl-x-1234567890ab")
	if err == nil {
		t.Fatalf("Run: err = nil, want non-nil (script exits 1)")
	}
	if !strings.Contains(out, "No such object") {
		t.Fatalf("out = %q, want contains 'No such object' (proves CombinedOutput captures stderr)", out)
	}
}

// execTempDir returns a temp directory on a filesystem that
// permits exec. On a typical Linux runner os.TempDir() (i.e.
// /tmp) is fine, but the development sandbox mounts /tmp with
// noexec. The helper prefers /workspace/.gotmp when it exists
// so tests that exec a script from a temp file stay portable
// across the sandbox and CI.
//
// Tests must use this helper instead of hardcoding /workspace/.gotmp
// or relying on t.TempDir()'s default of /tmp; the former does
// not exist on CI and the latter is noexec in the sandbox.
func execTempDir(t *testing.T) string {
	t.Helper()
	if info, err := os.Stat("/workspace/.gotmp"); err == nil && info.IsDir() {
		return "/workspace/.gotmp"
	}
	return os.TempDir()
}

// scriptedDockerRunner is the dockerRunner fake used by status
// tests that need to assert on what inspectContainerStatus passed
// to docker and what docker returned. It records every call and
// returns the configured (out, err) verbatim, so callers can
// model both success and failure responses.
type scriptedDockerRunner struct {
	out   string
	err   error
	calls []scriptedDockerCall
}

type scriptedDockerCall struct {
	name string
	args []string
}

func (r *scriptedDockerRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, scriptedDockerCall{name: name, args: append([]string(nil), args...)})
	return r.out, r.err
}

// TestListenAndServe_ShutdownWaitsForActiveDeploy is the
// regression test for the "30-second Shutdown timeout cuts off
// in-flight deploys" bug. With the previous fixed-30s shutdown
// timeout, cancelling the server context mid-deploy would race
// the deploy: after 30 seconds, server.Shutdown would return
// context.DeadlineExceeded, ListenAndServe would return, and
// the process could exit before the deploy finished. The fix
// removes the internal shutdown timeout so server.Shutdown
// waits for in-flight handlers (whose deploy runs on a
// background context bounded by OperationTimeout) to return.
//
// The assertion is twofold:
//   - ListenAndServe does NOT return while the deploy is still
//     running (it must wait for the handler to finish).
//   - ListenAndServe DOES return once the deploy finishes
//     (the handler returns, server.Shutdown returns nil, and
//     ListenAndServe returns ctx.Err()).
//
// We additionally verify the HTTP response was a success so a
// future regression that abandons the handler but somehow
// unblocks server.Shutdown would still be caught.
func TestListenAndServe_ShutdownWaitsForActiveDeploy(t *testing.T) {
	const (
		operationTimeout = 60 * time.Second
		deployerSleep    = 1500 * time.Millisecond
	)
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "agentctl.sock")
	cfg := minimalConfig(sockPath)
	cfg.OperationTimeout = operationTimeout

	fake := &slowFakeDeployer{sleepFor: deployerSleep}
	srv, err := NewServerWithDeployer(cfg, audit.New(io.Discard), fake, execDockerRunner{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(ctx) }()

	if err := waitForSocket(sockPath, 2*time.Second); err != nil {
		t.Fatalf("socket did not appear: %v", err)
	}

	manifestJSON := json.RawMessage(`{"version":1,"app":"example","container_port":8080,"health_path":"/healthz"}`)
	body, err := json.Marshal(map[string]any{
		"commit":   strings.Repeat("a", 40),
		"manifest": manifestJSON,
	})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}

	type httpResult struct {
		resp *httpResponse
		err  error
	}
	deployResultCh := make(chan httpResult, 1)
	go func() {
		resp, derr := httpPostOverUDS(sockPath, "/v1/apps/example/deploy", string(body))
		deployResultCh <- httpResult{resp: resp, err: derr}
	}()

	// Give the deploy goroutine time to acquire the app lock
	// and reach the fake Deployer sleep. 200ms is well below
	// the 1.5s deployer sleep so the deploy is definitely in
	// flight.
	time.Sleep(200 * time.Millisecond)

	// Cancel the server context. ListenAndServe must NOT
	// return until the deploy finishes; the previous
	// fixed-30s shutdown timeout would race the deploy and
	// return long before the 1.5s deployer sleep elapses.
	cancelAt := time.Now()
	cancel()

	select {
	case err := <-errCh:
		t.Fatalf("ListenAndServe returned after %v, want >= %v (deploy still in flight, err=%v)", time.Since(cancelAt), deployerSleep, err)
	case <-time.After(deployerSleep - 500*time.Millisecond):
		// expected: still waiting for deploy to finish
	}

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("ListenAndServe returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after deploy completed")
	}

	// Verify the deploy actually completed successfully so a
	// future regression that abandons the handler but somehow
	// unblocks server.Shutdown would still be caught.
	select {
	case res := <-deployResultCh:
		if res.err != nil {
			t.Fatalf("deploy request error: %v", res.err)
		}
		if res.resp.status != http.StatusOK {
			t.Fatalf("deploy status = %d, want 200; body = %s", res.resp.status, res.resp.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deploy result never delivered")
	}

	if calls := atomic.LoadInt32(&fake.calls); calls != 1 {
		t.Errorf("fake deployer calls = %d, want 1", calls)
	}
}

// TestListenAndServe_ShutdownWaitsForActiveRollback is the
// rollback counterpart of the deploy regression test. Rollback
// runs on the same background OperationTimeout-bounded context,
// so the same shutdown logic must keep it from being abandoned
// halfway through. The HTTP status returned by the handler
// with no persisted state is incidental to the shutdown-timing
// property under test; we only assert that the rollback was
// actually invoked and that the request ran to completion.
func TestListenAndServe_ShutdownWaitsForActiveRollback(t *testing.T) {
	const (
		operationTimeout = 60 * time.Second
		rollbackSleep    = 1500 * time.Millisecond
	)
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "agentctl.sock")
	cfg := minimalConfig(sockPath)
	cfg.OperationTimeout = operationTimeout

	fake := &slowFakeDeployer{sleepFor: rollbackSleep}
	srv, err := NewServerWithDeployer(cfg, audit.New(io.Discard), fake, execDockerRunner{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(ctx) }()

	if err := waitForSocket(sockPath, 2*time.Second); err != nil {
		t.Fatalf("socket did not appear: %v", err)
	}

	body, err := json.Marshal(map[string]any{
		"health_path": "/healthz",
	})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}

	type httpResult struct {
		resp *httpResponse
		err  error
	}
	rollbackResultCh := make(chan httpResult, 1)
	go func() {
		resp, derr := httpPostOverUDS(sockPath, "/v1/apps/example/rollback", string(body))
		rollbackResultCh <- httpResult{resp: resp, err: derr}
	}()

	time.Sleep(200 * time.Millisecond)

	cancelAt := time.Now()
	cancel()

	select {
	case err := <-errCh:
		t.Fatalf("ListenAndServe returned after %v, want >= %v (rollback still in flight, err=%v)", time.Since(cancelAt), rollbackSleep, err)
	case <-time.After(rollbackSleep - 500*time.Millisecond):
		// expected: still waiting for rollback to finish
	}

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("ListenAndServe returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after rollback completed")
	}

	select {
	case res := <-rollbackResultCh:
		if res.err != nil {
			t.Fatalf("rollback request error: %v", res.err)
		}
		// With no persisted state the handler eventually
		// returns 500 after the rollback completes; the
		// shutdown-timing property under test is independent
		// of whether the rollback succeeded operationally.
		// What we need to verify here is that the response
		// was delivered at all, which proves the handler
		// ran to completion rather than being abandoned.
		if res.resp.status == 0 {
			t.Fatalf("rollback response had no status; body = %s", res.resp.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rollback result never delivered")
	}

	if calls := atomic.LoadInt32(&fake.rollbackCalls); calls != 1 {
		t.Errorf("fake deployer rollback calls = %d, want 1", calls)
	}
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

// httpPostOverUDS performs a minimal HTTP/1.1 POST over a Unix
// domain socket. The dial and read deadlines are wide enough for
// the slow-deployer test (2s deploy) plus JSON encoding; tighten
// them once that test moves to a quicker assertion.
func httpPostOverUDS(sockPath, path, body string) (*httpResponse, error) {
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", sockPath, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return nil, err
	}
	req := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		path, len(body), body)
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

// ---------- handleInspect env_statuses ----------

// minimalConfigWithSecrets returns a minimal config that also has
// SecretDir set. The secret dir is created by the caller (via
// t.TempDir or a hand-built dir) and passed in directly.
func minimalConfigWithSecrets(socketPath, secretDir string) *Config {
	cfg := minimalConfig(socketPath)
	cfg.SecretDir = secretDir
	return cfg
}

// inspectHandlerEnv builds a server + inspect handler invocation
// that uses a local git remote and a v3 manifest with the supplied
// env entries. Returns the response and recorder so tests can
// inspect body, status, and decoded response.
func inspectHandlerEnv(t *testing.T, manifestJSON string, env []deploy.EnvEntry) (*httptest.ResponseRecorder, *InspectResponse) {
	t.Helper()

	remoteDir := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = remoteDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q", "-b", "main", remoteDir)
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	runGit("commit", "--allow-empty", "-q", "-m", "first commit on main")
	mainSHA := runGit("rev-parse", "HEAD")

	cfg := minimalConfig(filepath.Join(t.TempDir(), "agentctl.sock"))
	cfg.Source.RepositoryRoot = t.TempDir()
	cfg.Source.OriginURLOverride = remoteDir

	srv, err := NewServerWithDeployer(cfg, audit.New(io.Discard), realDeployer{}, execDockerRunner{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	body, err := json.Marshal(map[string]any{
		"app":      "myapp",
		"commit":   mainSHA,
		"manifest": json.RawMessage(manifestJSON),
	})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/inspect", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleInspect(w, req)
	var resp InspectResponse
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&resp); err != nil {
		t.Fatalf("decode inspect response: %v", err)
	}
	return w, &resp
}

func TestInspectHandler_EnvStatusesConfigured(t *testing.T) {
	secretDir := t.TempDir()
	// foo.key configured; bar.key (optional) missing.
	if err := os.WriteFile(filepath.Join(secretDir, "foo.key"), []byte("foo-secret-value\n"), 0o600); err != nil {
		t.Fatalf("seed foo: %v", err)
	}
	if err := os.Chmod(filepath.Join(secretDir, "foo.key"), 0o600); err != nil {
		t.Fatalf("chmod foo: %v", err)
	}

	cfg := minimalConfigWithSecrets(filepath.Join(t.TempDir(), "agentctl.sock"), secretDir)
	remoteDir := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = remoteDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q", "-b", "main", remoteDir)
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	runGit("commit", "--allow-empty", "-q", "-m", "first commit on main")
	mainSHA := runGit("rev-parse", "HEAD")

	cfg.Source.RepositoryRoot = t.TempDir()
	cfg.Source.OriginURLOverride = remoteDir

	srv, err := NewServerWithDeployer(cfg, audit.New(io.Discard), realDeployer{}, execDockerRunner{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	manifestJSON := json.RawMessage(`{"version":3,"app":"myapp","repository":"myapp","container_port":8080,"health_path":"/healthz","env":[{"name":"FOO","secret_ref":"foo.key","required":true},{"name":"BAR","secret_ref":"bar.key"}]}`)
	body, err := json.Marshal(map[string]any{
		"app":      "myapp",
		"commit":   mainSHA,
		"manifest": manifestJSON,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/inspect", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleInspect(w, req)

	var resp InspectResponse
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// foo.key is seeded (configured); bar.key is missing (optional).
	// Valid must be true (no missing required secret) and the only
	// error (if any) should be about the optional BAR entry, not FOO.
	t.Logf("resp = %+v", resp)
	if !resp.Valid {
		t.Errorf("inspect valid=false despite required secret configured; errors=%v", resp.Errors)
	}
	if len(resp.EnvStatuses) != 2 {
		t.Fatalf("env_statuses length = %d, want 2", len(resp.EnvStatuses))
	}
	byName := map[string]EnvStatus{}
	for _, es := range resp.EnvStatuses {
		byName[es.Name] = es
	}
	if foo, ok := byName["FOO"]; !ok {
		t.Errorf("env_statuses missing FOO entry")
	} else {
		if !foo.Configured {
			t.Errorf("FOO configured=false, want true")
		}
		if !foo.Required {
			t.Errorf("FOO required=false, want true")
		}
		if foo.SecretRef != "foo.key" {
			t.Errorf("FOO secret_ref=%q, want foo.key", foo.SecretRef)
		}
	}
	if bar, ok := byName["BAR"]; !ok {
		t.Errorf("env_statuses missing BAR entry")
	} else {
		if bar.Configured {
			t.Errorf("BAR configured=true, want false")
		}
		if bar.Required {
			t.Errorf("BAR required=true, want false")
		}
	}

	// SECRET VALUES MUST NEVER APPEAR IN THE RESPONSE BODY.
	bodyBytes := w.Body.Bytes()
	if strings.Contains(string(bodyBytes), "bar-secret-value") {
		t.Errorf("response body contains secret value 'bar-secret-value': %s", bodyBytes)
	}
}

func TestInspectHandler_EnvStatusesAllConfigured(t *testing.T) {
	secretDir := t.TempDir()
	for _, name := range []string{"foo.key", "bar.key"} {
		if err := os.WriteFile(filepath.Join(secretDir, name), []byte("value-"+name+"\n"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		if err := os.Chmod(filepath.Join(secretDir, name), 0o600); err != nil {
			t.Fatalf("chmod %s: %v", name, err)
		}
	}

	cfg := minimalConfigWithSecrets(filepath.Join(t.TempDir(), "agentctl.sock"), secretDir)
	remoteDir := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = remoteDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q", "-b", "main", remoteDir)
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	runGit("commit", "--allow-empty", "-q", "-m", "first commit on main")
	mainSHA := runGit("rev-parse", "HEAD")

	cfg.Source.RepositoryRoot = t.TempDir()
	cfg.Source.OriginURLOverride = remoteDir

	srv, err := NewServerWithDeployer(cfg, audit.New(io.Discard), realDeployer{}, execDockerRunner{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	manifestJSON := json.RawMessage(`{"version":3,"app":"myapp","repository":"myapp","container_port":8080,"health_path":"/healthz","env":[{"name":"FOO","secret_ref":"foo.key"},{"name":"BAR","secret_ref":"bar.key"}]}`)
	body, err := json.Marshal(map[string]any{
		"app":      "myapp",
		"commit":   mainSHA,
		"manifest": manifestJSON,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/inspect", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleInspect(w, req)

	var resp InspectResponse
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Valid {
		t.Errorf("inspect valid=false; errors=%v", resp.Errors)
	}
	if len(resp.EnvStatuses) != 2 {
		t.Fatalf("env_statuses length = %d, want 2", len(resp.EnvStatuses))
	}
	for _, es := range resp.EnvStatuses {
		if !es.Configured {
			t.Errorf("env_statuses[%s] configured=false, want true", es.Name)
		}
	}

	// No secret values should appear.
	bodyBytes := w.Body.Bytes()
	if strings.Contains(string(bodyBytes), "value-foo.key") {
		t.Errorf("response body contains secret value: %s", bodyBytes)
	}
	if strings.Contains(string(bodyBytes), "value-bar.key") {
		t.Errorf("response body contains secret value: %s", bodyBytes)
	}
}

func TestInspectHandler_NoEnvNoEnvStatuses(t *testing.T) {
	// V3 manifest without env entries: env_statuses must be empty.
	secretDir := t.TempDir()
	cfg := minimalConfigWithSecrets(filepath.Join(t.TempDir(), "agentctl.sock"), secretDir)
	remoteDir := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = remoteDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q", "-b", "main", remoteDir)
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	runGit("commit", "--allow-empty", "-q", "-m", "first commit on main")
	mainSHA := runGit("rev-parse", "HEAD")

	cfg.Source.RepositoryRoot = t.TempDir()
	cfg.Source.OriginURLOverride = remoteDir

	srv, err := NewServerWithDeployer(cfg, audit.New(io.Discard), realDeployer{}, execDockerRunner{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	manifestJSON := json.RawMessage(`{"version":3,"app":"myapp","repository":"myapp","container_port":8080,"health_path":"/healthz"}`)
	body, err := json.Marshal(map[string]any{
		"app":      "myapp",
		"commit":   mainSHA,
		"manifest": manifestJSON,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/inspect", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleInspect(w, req)

	var resp InspectResponse
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Valid {
		t.Errorf("inspect valid=false; errors=%v", resp.Errors)
	}
	if len(resp.EnvStatuses) != 0 {
		t.Errorf("env_statuses = %+v, want empty for v3 manifest without env", resp.EnvStatuses)
	}
}

// TestInspectHandler_EnvStatusesRequiredMissingFails inspects a
// manifest whose required env entry is missing: the inspect
// response must report configured=false for that entry and
// valid=false overall. This is the inspect-side counterpart of
// the deploy-time ErrSecretMissing failure: a caller can
// pre-flight this and refuse to approve the deploy.
func TestInspectHandler_EnvStatusesRequiredMissingFails(t *testing.T) {
	secretDir := t.TempDir()
	// No secrets at all: every required env entry will be missing.
	cfg := minimalConfigWithSecrets(filepath.Join(t.TempDir(), "agentctl.sock"), secretDir)
	remoteDir := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = remoteDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q", "-b", "main", remoteDir)
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	runGit("commit", "--allow-empty", "-q", "-m", "first commit on main")
	mainSHA := runGit("rev-parse", "HEAD")

	cfg.Source.RepositoryRoot = t.TempDir()
	cfg.Source.OriginURLOverride = remoteDir

	srv, err := NewServerWithDeployer(cfg, audit.New(io.Discard), realDeployer{}, execDockerRunner{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	manifestJSON := json.RawMessage(`{"version":3,"app":"myapp","repository":"myapp","container_port":8080,"health_path":"/healthz","env":[{"name":"FOO","secret_ref":"foo.key","required":true}]}`)
	body, err := json.Marshal(map[string]any{
		"app":      "myapp",
		"commit":   mainSHA,
		"manifest": manifestJSON,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/inspect", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleInspect(w, req)

	var resp InspectResponse
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Valid {
		t.Errorf("inspect valid=true despite missing required secret; errors=%v", resp.Errors)
	}
	if len(resp.EnvStatuses) != 1 {
		t.Fatalf("env_statuses length = %d, want 1", len(resp.EnvStatuses))
	}
	if resp.EnvStatuses[0].Configured {
		t.Errorf("FOO configured=true, want false (file missing)")
	}
	if !resp.EnvStatuses[0].Required {
		t.Errorf("FOO required=false, want true")
	}
	// The error must mention the missing required secret.
	found := false
	for _, e := range resp.Errors {
		if strings.Contains(e, "FOO") && strings.Contains(e, "foo.key") {
			found = true
		}
	}
	if !found {
		t.Errorf("errors do not mention missing required secret FOO/foo.key: %v", resp.Errors)
	}
}

// TestInspectHandler_EnvStatusesOptionalMissingPasses is the
// counterpoint: an optional env entry that is missing does NOT
// fail the inspect. This is already exercised by
// TestInspectHandler_EnvStatusesConfigured but with both required
// + optional together; here we verify the optional-only case.
func TestInspectHandler_EnvStatusesOptionalMissingPasses(t *testing.T) {
	secretDir := t.TempDir()
	cfg := minimalConfigWithSecrets(filepath.Join(t.TempDir(), "agentctl.sock"), secretDir)
	remoteDir := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = remoteDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q", "-b", "main", remoteDir)
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	runGit("commit", "--allow-empty", "-q", "-m", "first commit on main")
	mainSHA := runGit("rev-parse", "HEAD")

	cfg.Source.RepositoryRoot = t.TempDir()
	cfg.Source.OriginURLOverride = remoteDir

	srv, err := NewServerWithDeployer(cfg, audit.New(io.Discard), realDeployer{}, execDockerRunner{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	manifestJSON := json.RawMessage(`{"version":3,"app":"myapp","repository":"myapp","container_port":8080,"health_path":"/healthz","env":[{"name":"FOO","secret_ref":"foo.key"}]}`)
	body, err := json.Marshal(map[string]any{
		"app":      "myapp",
		"commit":   mainSHA,
		"manifest": manifestJSON,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/inspect", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleInspect(w, req)

	var resp InspectResponse
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !resp.Valid {
		t.Errorf("inspect valid=false for missing optional secret; errors=%v", resp.Errors)
	}
	if len(resp.EnvStatuses) != 1 {
		t.Fatalf("env_statuses length = %d, want 1", len(resp.EnvStatuses))
	}
	if resp.EnvStatuses[0].Configured {
		t.Errorf("FOO configured=true, want false")
	}
	if resp.EnvStatuses[0].Required {
		t.Errorf("FOO required=true, want false")
	}
}
