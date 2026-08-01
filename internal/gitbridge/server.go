package gitbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"

	"github.com/simons-agent-space/agentctl/internal/audit"
	"github.com/simons-agent-space/agentctl/internal/github"
	"github.com/simons-agent-space/agentctl/internal/policy"
)

// Server is the broker: it owns the UDS listener, the token issuer, and
// the audit logger. It exposes a single POST /token endpoint over UDS
// plus a /healthz liveness check.
type Server struct {
	cfg    *Config
	issuer *tokenIssuer
	log    *audit.Logger
}

// NewServer validates the config and prepares a Server ready to listen.
// The private key is loaded during this call.
func NewServer(cfg *Config, ghc *github.Client, log *audit.Logger) (*Server, error) {
	issuer, err := newTokenIssuer(cfg, ghc)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, issuer: issuer, log: log}, nil
}

// ListenAndServe creates the Unix domain socket and serves until ctx is
// cancelled. The socket is created with mode 0660 by default; the caller
// can configure the mode and group via the config file.
func (s *Server) ListenAndServe(ctx context.Context) error {
	// Remove any stale socket from a previous run.
	if err := os.Remove(s.cfg.SocketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	listener, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.SocketPath, err)
	}
	defer listener.Close()
	defer os.Remove(s.cfg.SocketPath)

	if s.cfg.SocketMode != "" {
		mode, err := strconv.ParseUint(s.cfg.SocketMode, 8, 32)
		if err != nil {
			return fmt.Errorf("parse socket_mode: %w", err)
		}
		if err := os.Chmod(s.cfg.SocketPath, os.FileMode(mode)); err != nil {
			return fmt.Errorf("chmod socket: %w", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/token", s.handleToken)
	mux.HandleFunc("/healthz", s.handleHealthz)

	server := &http.Server{Handler: mux}

	errCh := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		_ = server.Shutdown(context.Background())
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// handleHealthz returns 200 OK for liveness checks. It does not require
// any configuration and does not contact the GitHub API.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleToken processes one mint request. The handler is intentionally
// simple: every step is logged via the audit logger and the private key,
// JWT, and installation token never appear in any log line.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(errorResponse{
			Error: "method not allowed",
			Code:  "METHOD_NOT_ALLOWED",
		})
		return
	}

	repo, prof, verr := decodeRequest(r.Body)
	if verr != nil {
		s.auditReject(repo, prof, verr)
		writeError(w, verr)
		return
	}

	resolved, verr := s.cfg.authorize(repo, prof)
	if verr != nil {
		s.auditReject(repo, prof, verr)
		writeError(w, verr)
		return
	}

	s.log.Info("audit", map[string]any{
		"op":      "validated",
		"repo":    repo,
		"profile": string(prof),
	})

	token, exp, err := s.issuer.mintAndReturn(r.Context(), repo, resolved)
	if err != nil {
		ve := errInternal(err)
		s.auditReject(repo, prof, ve)
		writeError(w, ve)
		return
	}

	s.log.Info("audit", map[string]any{
		"op":         "minted",
		"repo":       repo,
		"profile":    string(prof),
		"expires_at": exp.UTC().Format("2006-01-02T15:04:05Z"),
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(response{
		Token:     token,
		ExpiresAt: exp.UTC().Format("2006-01-02T15:04:05Z"),
		Repo:      repo,
		Profile:   string(prof),
	})
}

// auditReject emits a single audit event for a rejected request. The
// reason is included but never the underlying secret material.
func (s *Server) auditReject(repo string, prof policy.Name, verr *validationError) {
	ev := &audit.Event{
		Op:      "rejected",
		Repo:    repo,
		Profile: string(prof),
		Reason:  verr.Error(),
	}
	ev.Emit(s.log)
}
