package scripts

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func scriptPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	p := filepath.Join(wd, "agentctld-wait.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("script not found at %s: %v", p, err)
	}
	return p
}

// TestAgentctldWait_Success verifies that the wait script exits 0
// when /healthz returns 200 OK over a Unix socket.
func TestAgentctldWait_Success(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}

	// Create a UDS socket that responds to /healthz with 200 OK.
	socketPath := filepath.Join(t.TempDir(), "agentctld.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	cmd := exec.Command("bash", scriptPath(t), "-t", "5", "-s", socketPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("expected exit 0, got %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
}

// TestAgentctldWait_Timeout verifies that the wait script exits 1
// with a diagnostic message when the socket never responds within
// the timeout.
func TestAgentctldWait_Timeout(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}

	// Point at a non-existent socket so the script always times out.
	socketPath := filepath.Join(t.TempDir(), "nonexistent.sock")

	cmd := exec.Command("bash", scriptPath(t), "-t", "1", "-s", socketPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatalf("expected exit 1, got 0\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "did not become ready") {
		t.Errorf("expected timeout message in stderr, got: %s", stderr.String())
	}
}

// TestAgentctldWait_BadArgs verifies that the wait script exits 2
// when given an invalid timeout.
func TestAgentctldWait_BadArgs(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	cmd := exec.Command("bash", scriptPath(t), "-t", "abc")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatalf("expected exit 2, got 0\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid timeout") {
		t.Errorf("expected invalid timeout message in stderr, got: %s", stderr.String())
	}
}

// TestAgentctldWait_MissingOptionValue verifies that the wait script
// exits 2 with a useful error when -t or -s is provided without a
// value (instead of failing under set -u).
func TestAgentctldWait_MissingOptionValue(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	for _, arg := range []string{"-t", "-s"} {
		arg := arg
		t.Run(arg, func(t *testing.T) {
			cmd := exec.Command("bash", scriptPath(t), arg)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			if err == nil {
				t.Fatalf("expected exit 2, got 0\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "missing value") {
				t.Errorf("expected 'missing value' message in stderr, got: %s", stderr.String())
			}
		})
	}
}
