package gitbridge

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/simons-agent-space/agentctl/internal/audit"
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
func NewServer(cfg *Config, ghc *Client, log *audit.Logger) (*Server, error) {
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

// auditReject emits a single WARN audit event for a rejected request.
// The reason is included but never the underlying secret material.
func (s *Server) auditReject(repo string, prof Name, verr *validationError) {
	s.log.Warn("audit", map[string]any{
		"op":      "rejected",
		"repo":    repo,
		"profile": string(prof),
		"reason":  verr.Error(),
	})
}

// ----------------------------------------------------------------------
// Token issuer (mint JWT, call GitHub API, return installation token)
// ----------------------------------------------------------------------

type tokenIssuer struct {
	appID        int64
	installation int64
	privateKey   *rsa.PrivateKey
	githubClient *Client
}

func newTokenIssuer(cfg *Config, ghc *Client) (*tokenIssuer, error) {
	key, err := loadPrivateKey(cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load private key: %w", err)
	}
	if key == nil {
		return nil, fmt.Errorf("nil private key")
	}
	return &tokenIssuer{
		appID:        cfg.AppID,
		installation: cfg.InstallationID,
		privateKey:   key,
		githubClient: ghc,
	}, nil
}

func (i *tokenIssuer) mintAndReturn(
	ctx context.Context,
	repoSlug string,
	prof Profile,
) (string, time.Time, error) {
	jwt, err := mintJWT(i.privateKey, i.appID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("mint jwt: %w", err)
	}
	perms := make([]Permission, 0, len(prof.Permissions))
	for _, p := range prof.Permissions {
		perms = append(perms, Permission{Scope: p.Scope, Access: p.Access})
	}
	req := AccessTokenRequest{
		Repositories: []string{repoSlug},
		Permissions:  perms,
	}
	resp, err := i.githubClient.MintInstallationToken(ctx, jwt, i.installation, req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("mint installation token: %w", err)
	}
	return resp.Token, resp.ExpiresAt, nil
}

// ----------------------------------------------------------------------
// JWT minting (RS256, in-process, no third-party deps)
// ----------------------------------------------------------------------

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type jwtPayload struct {
	Iat int64 `json:"iat"`
	Exp int64 `json:"exp"`
	Iss int64 `json:"iss"`
}

// loadPrivateKey reads a PEM-encoded RSA private key from path. Both
// PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE KEY") forms are accepted.
func loadPrivateKey(path string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("no PEM block found in private key file")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	rsaKey, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return rsaKey, nil
}

// mintJWT produces a fresh RS256 JWT for the GitHub App. The token is
// signed in-process; the resulting string is never logged by the broker.
// The function is called once per installation-token mint, so the JWT
// has the shortest possible lifetime.
func mintJWT(key *rsa.PrivateKey, appID int64) (string, error) {
	now := time.Now()
	hdr := jwtHeader{Alg: "RS256", Typ: "JWT"}
	pl := jwtPayload{
		Iat: now.Add(-60 * time.Second).Unix(),
		Exp: now.Add(9 * time.Minute).Unix(),
		Iss: appID,
	}
	hdrJSON, err := json.Marshal(hdr)
	if err != nil {
		return "", fmt.Errorf("marshal jwt header: %w", err)
	}
	plJSON, err := json.Marshal(pl)
	if err != nil {
		return "", fmt.Errorf("marshal jwt payload: %w", err)
	}
	hdrEnc := base64.RawURLEncoding.EncodeToString(hdrJSON)
	plEnc := base64.RawURLEncoding.EncodeToString(plJSON)
	signingInput := hdrEnc + "." + plEnc
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	sigEnc := base64.RawURLEncoding.EncodeToString(sig)
	return signingInput + "." + sigEnc, nil
}

// ----------------------------------------------------------------------
// Request validation (decode JSON, enforce allowlist and profile)
// ----------------------------------------------------------------------

type request struct {
	Repo    string `json:"repo"`
	Profile string `json:"profile"`
}

type response struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	Repo      string `json:"repository"`
	Profile   string `json:"profile"`
}

type errorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

type validationError struct {
	Code   string
	Reason string
}

func (v *validationError) Error() string {
	return fmt.Sprintf("%s: %s", v.Code, v.Reason)
}

func decodeRequest(r io.Reader) (string, Name, *validationError) {
	var req request
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return "", "", &validationError{Code: "BAD_REQUEST", Reason: "invalid JSON body"}
	}
	if req.Repo == "" {
		return "", "", &validationError{Code: "BAD_REQUEST", Reason: "repo is required"}
	}
	if req.Profile == "" {
		return "", "", &validationError{Code: "BAD_REQUEST", Reason: "profile is required"}
	}
	return req.Repo, Name(req.Profile), nil
}

// authorize checks the request against the configured allowlist and
// returns the resolved Profile or a validation error.
func (c *Config) authorize(repoSlug string, prof Name) (Profile, *validationError) {
	if prof != ProfileBuilder {
		return Profile{}, &validationError{
			Code:   "PROFILE_NOT_ALLOWED",
			Reason: fmt.Sprintf("only %q is accepted", ProfileBuilder),
		}
	}
	if !c.IsAllowed(repoSlug) {
		return Profile{}, &validationError{
			Code:   "REPO_NOT_ALLOWED",
			Reason: "repository not in allowlist or wrong organisation",
		}
	}
	profile, err := Resolve(prof)
	if err != nil {
		return Profile{}, &validationError{
			Code:   "PROFILE_NOT_ALLOWED",
			Reason: err.Error(),
		}
	}
	return profile, nil
}

// writeError serialises err as an errorResponse with the appropriate HTTP
// status. It does not log; the caller is responsible for audit logging.
func writeError(w http.ResponseWriter, err *validationError) {
	status := http.StatusBadRequest
	switch err.Code {
	case "REPO_NOT_ALLOWED", "PROFILE_NOT_ALLOWED":
		status = http.StatusForbidden
	case "INTERNAL":
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: err.Reason, Code: err.Code})
}

func errInternal(err error) *validationError {
	var ve *validationError
	if errors.As(err, &ve) {
		return ve
	}
	return &validationError{Code: "INTERNAL", Reason: err.Error()}
}
