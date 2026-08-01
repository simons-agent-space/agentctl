package gitbridge

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/simons-agent-space/agentctl/internal/audit"
)

// makeTestServer returns a Server whose GitHub API is the supplied
// httptest handler, plus a buffer of the audit log.
func makeTestServer(t *testing.T, ghHandler http.HandlerFunc) (*Server, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "key.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(keyPath, pemBytes, 0600); err != nil {
		t.Fatal(err)
	}
	ghServer := httptest.NewServer(ghHandler)
	t.Cleanup(ghServer.Close)
	runDir := filepath.Join(dir, "run")
	if err := os.MkdirAll(runDir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		AppID:          12345,
		InstallationID: 67890,
		PrivateKeyPath: keyPath,
		AllowedOrg:     "simons-agent-space",
		AllowedRepos:   []string{"agentctl"},
		SocketPath:     filepath.Join(runDir, "gitbridge.sock"),
	}
	ghClient := NewClientWithBase(ghServer.URL)
	var logBuf bytes.Buffer
	log := audit.New(&logBuf)
	srv, err := NewServer(cfg, ghClient, log)
	if err != nil {
		t.Fatal(err)
	}
	return srv, &logBuf
}

// ----------------------------------------------------------------------
// End-to-end server tests
// ----------------------------------------------------------------------

func TestServer_Healthz(t *testing.T) {
	srv, _ := makeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub API should not be called for healthz")
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.handleHealthz(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestServer_HealthzRejectsNonGET(t *testing.T) {
	srv, _ := makeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub API should not be called for healthz")
	})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/healthz", nil)
		srv.handleHealthz(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /healthz: expected 405, got %d", method, rec.Code)
		}
	}
}

func TestServer_RejectsBadMethod(t *testing.T) {
	srv, _ := makeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub API should not be called")
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/token", strings.NewReader(""))
	srv.handleToken(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestServer_RejectsBadJSON(t *testing.T) {
	srv, _ := makeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub API should not be called")
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader("not json"))
	srv.handleToken(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestServer_RejectsTrailingJSON(t *testing.T) {
	srv, _ := makeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub API must not be called when the request body has trailing data")
	})
	body := strings.NewReader(`{"repo":"simons-agent-space/agentctl","profile":"builder"}{"sneaky":"object"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/token", body)
	srv.handleToken(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	var er errorResponse
	if err := json.NewDecoder(rec.Body).Decode(&er); err != nil {
		t.Fatal(err)
	}
	if er.Code != "BAD_REQUEST" {
		t.Errorf("code = %q, want BAD_REQUEST", er.Code)
	}
	if !strings.Contains(strings.ToLower(er.Error), "trailing") {
		t.Errorf("error should mention trailing data, got %q", er.Error)
	}
}

func TestServer_RejectsRepoNotAllowed(t *testing.T) {
	srv, _ := makeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub API should not be called for disallowed repo")
	})
	body := strings.NewReader(`{"repo":"other-org/foo","profile":"builder"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/token", body)
	srv.handleToken(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
	var er errorResponse
	if err := json.NewDecoder(rec.Body).Decode(&er); err != nil {
		t.Fatal(err)
	}
	if er.Code != "REPO_NOT_ALLOWED" {
		t.Errorf("code = %q", er.Code)
	}
}

func TestServer_RejectsWrongProfile(t *testing.T) {
	srv, _ := makeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub API should not be called for disallowed profile")
	})
	body := strings.NewReader(`{"repo":"simons-agent-space/agentctl","profile":"admin"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/token", body)
	srv.handleToken(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
	var er errorResponse
	if err := json.NewDecoder(rec.Body).Decode(&er); err != nil {
		t.Fatal(err)
	}
	if er.Code != "PROFILE_NOT_ALLOWED" {
		t.Errorf("code = %q", er.Code)
	}
}

func TestServer_LogsRejection(t *testing.T) {
	srv, logBuf := makeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub API should not be called")
	})
	body := strings.NewReader(`{"repo":"simons-agent-space/agentctl","profile":"admin"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/token", body)
	srv.handleToken(rec, req)
	if !strings.Contains(logBuf.String(), `"op":"rejected"`) {
		t.Errorf("rejected event missing in log: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"reason"`) {
		t.Errorf("reason missing in rejection log: %s", logBuf.String())
	}
}

func TestServer_GitHubErrorReturnsGenericInternal(t *testing.T) {
	const fakeSecret = "internal-stack-secret-ABC123-leaked-from-upstream"
	ghHandler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"`+fakeSecret+`"}`)
	}
	srv, logBuf := makeTestServer(t, ghHandler)
	body := strings.NewReader(`{"repo":"simons-agent-space/agentctl","profile":"builder"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/token", body)
	srv.handleToken(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}

	bodyStr := rec.Body.String()
	if strings.Contains(bodyStr, fakeSecret) {
		t.Errorf("fake upstream secret leaked to UDS caller: %s", bodyStr)
	}
	if strings.Contains(logBuf.String(), fakeSecret) {
		t.Errorf("fake upstream secret leaked to audit log: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"error_class":"upstream"`) {
		t.Errorf("audit log should classify error as upstream: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"http_status":500`) {
		t.Errorf("audit log should record http_status 500: %s", logBuf.String())
	}

	var er errorResponse
	if err := json.NewDecoder(strings.NewReader(bodyStr)).Decode(&er); err != nil {
		t.Fatal(err)
	}
	if er.Code != "INTERNAL" {
		t.Errorf("code = %q, want INTERNAL", er.Code)
	}
	if er.Error != "internal error" {
		t.Errorf("error message should be generic, got %q", er.Error)
	}
}

// ----------------------------------------------------------------------
// Contract tests for the GitHub access-tokens request body
// (these guard the broker against silent contract regressions)
// ----------------------------------------------------------------------

func TestGitHubRequest_ApiVersionHeader(t *testing.T) {
	var capturedVersion string
	ghHandler := func(w http.ResponseWriter, r *http.Request) {
		capturedVersion = r.Header.Get("X-GitHub-Api-Version")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "***",
			"expires_at": time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	srv, _ := makeTestServer(t, ghHandler)
	body := strings.NewReader(`{"repo":"simons-agent-space/agentctl","profile":"builder"}`)
	rec := httptest.NewRecorder()
	srv.handleToken(rec, httptest.NewRequest(http.MethodPost, "/token", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	const wantVersion = "2026-03-10"
	if capturedVersion != wantVersion {
		t.Errorf("X-GitHub-Api-Version = %q, want %q", capturedVersion, wantVersion)
	}
}

func TestGitHubRequest_EndpointPath(t *testing.T) {
	var capturedPath string
	var capturedMethod string
	ghHandler := func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "***",
			"expires_at": time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	srv, _ := makeTestServer(t, ghHandler)
	body := strings.NewReader(`{"repo":"simons-agent-space/agentctl","profile":"builder"}`)
	rec := httptest.NewRecorder()
	srv.handleToken(rec, httptest.NewRequest(http.MethodPost, "/token", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if capturedMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", capturedMethod)
	}
	const wantPath = "/app/installations/67890/access_tokens"
	if capturedPath != wantPath {
		t.Errorf("path = %q, want %q", capturedPath, wantPath)
	}
}

func TestGitHubRequest_RepositoriesContainsRepoName(t *testing.T) {
	var capturedRepos []string
	ghHandler := func(w http.ResponseWriter, r *http.Request) {
		var parsed struct {
			Repositories []string `json:"repositories"`
		}
		_ = json.NewDecoder(r.Body).Decode(&parsed)
		capturedRepos = parsed.Repositories
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "***",
			"expires_at": time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	srv, _ := makeTestServer(t, ghHandler)
	body := strings.NewReader(`{"repo":"simons-agent-space/agentctl","profile":"builder"}`)
	rec := httptest.NewRecorder()
	srv.handleToken(rec, httptest.NewRequest(http.MethodPost, "/token", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(capturedRepos) != 1 {
		t.Fatalf("repositories = %v, want exactly one entry", capturedRepos)
	}
	if capturedRepos[0] != "agentctl" {
		t.Errorf("repositories[0] = %q, want %q (repo name only, no org prefix)", capturedRepos[0], "agentctl")
	}
	for _, r := range capturedRepos {
		if strings.Contains(r, "/") {
			t.Errorf("repository %q must not contain '/'; the broker should send the short name", r)
		}
	}
}

func TestGitHubRequest_PermissionsIsJSONObject(t *testing.T) {
	ghHandler := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var generic map[string]any
		if err := json.Unmarshal(raw, &generic); err != nil {
			t.Errorf("decode body as generic object: %v", err)
			return
		}
		perms, present := generic["permissions"]
		if !present {
			t.Errorf("body missing 'permissions' key: %s", string(raw))
			return
		}
		if _, isArray := perms.([]any); isArray {
			t.Errorf("permissions must be a JSON object, not an array; got %s", string(raw))
		}
		if _, isObject := perms.(map[string]any); !isObject {
			t.Errorf("permissions must be a JSON object; got type %T", perms)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "***",
			"expires_at": time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	srv, _ := makeTestServer(t, ghHandler)
	body := strings.NewReader(`{"repo":"simons-agent-space/agentctl","profile":"builder"}`)
	rec := httptest.NewRecorder()
	srv.handleToken(rec, httptest.NewRequest(http.MethodPost, "/token", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGitHubRequest_OnlyBuilderPermissionsRequested(t *testing.T) {
	var captured map[string]any
	ghHandler := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "***",
			"expires_at": time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	srv, _ := makeTestServer(t, ghHandler)
	body := strings.NewReader(`{"repo":"simons-agent-space/agentctl","profile":"builder"}`)
	rec := httptest.NewRecorder()
	srv.handleToken(rec, httptest.NewRequest(http.MethodPost, "/token", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	perms, ok := captured["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions not a JSON object: %+v", captured["permissions"])
	}

	wantScopes := map[string]string{
		"contents":      "write",
		"pull_requests": "write",
		"metadata":      "read",
	}
	if len(perms) != len(wantScopes) {
		t.Errorf("got %d permissions, want %d: %v", len(perms), len(wantScopes), perms)
	}
	for scope, level := range wantScopes {
		got, present := perms[scope]
		if !present {
			t.Errorf("missing permission %q", scope)
			continue
		}
		if got != level {
			t.Errorf("permission %q = %v, want %q", scope, got, level)
		}
	}

	// Scopes the agent does not need must not be requested.
	forbidden := []string{"checks", "statuses", "issues", "actions", "deployments", "packages", "members", "administration"}
	for _, scope := range forbidden {
		if _, present := perms[scope]; present {
			t.Errorf("forbidden permission %q must not be requested; got: %v", scope, perms)
		}
	}
}

// ----------------------------------------------------------------------
// Socket mode
// ----------------------------------------------------------------------

func TestDefaultSocketModeConstant(t *testing.T) {
	if DefaultSocketMode != 0660 {
		t.Errorf("DefaultSocketMode = %o, want 0660", DefaultSocketMode)
	}
}

// ----------------------------------------------------------------------
// JWT key loading and minting tests
// ----------------------------------------------------------------------

func TestLoadPrivateKeyPKCS1(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	path := writeTempKey(t, pemBytes)
	t.Cleanup(func() { _ = os.Remove(path) })

	loaded, err := loadPrivateKey(path)
	if err != nil {
		t.Fatalf("loadPrivateKey: %v", err)
	}
	if loaded.D.Cmp(key.D) != 0 {
		t.Errorf("loaded key D does not match original")
	}
}

func TestLoadPrivateKeyPKCS8(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	path := writeTempKey(t, pemBytes)
	t.Cleanup(func() { _ = os.Remove(path) })

	if _, err := loadPrivateKey(path); err != nil {
		t.Fatalf("loadPrivateKey PKCS#8: %v", err)
	}
}

func TestLoadPrivateKeyBadPEM(t *testing.T) {
	path := writeTempKey(t, []byte("not a pem file"))
	t.Cleanup(func() { _ = os.Remove(path) })
	if _, err := loadPrivateKey(path); err == nil {
		t.Errorf("expected error for bad PEM")
	}
}

func TestLoadPrivateKeyMissing(t *testing.T) {
	if _, err := loadPrivateKey("/nonexistent/key.pem"); err == nil {
		t.Errorf("expected error for missing file")
	}
}

func TestMintJWT_StructureAndTiming(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := mintJWT(key, 12345)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT must have 3 parts, got %d", len(parts))
	}
	hdr, pl := decodeJWTForTest(t, tok)
	if hdr["alg"] != "RS256" {
		t.Errorf("alg = %v, want RS256", hdr["alg"])
	}
	if hdr["typ"] != "JWT" {
		t.Errorf("typ = %v, want JWT", hdr["typ"])
	}
	iat := int64(pl["iat"].(float64))
	exp := int64(pl["exp"].(float64))
	iss := int64(pl["iss"].(float64))
	now := time.Now().Unix()
	if iat > now {
		t.Errorf("iat in future: %d > %d", iat, now)
	}
	if exp <= now {
		t.Errorf("exp not in future: %d <= %d", exp, now)
	}
	if exp-iat < 300 {
		t.Errorf("exp-iat too small: %d", exp-iat)
	}
	if exp-iat > 600 {
		t.Errorf("exp-iat too large: %d", exp-iat)
	}
	if iss != 12345 {
		t.Errorf("iss = %d, want 12345", iss)
	}
}

func TestMintJWT_NeverContainsKeyMaterial(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := mintJWT(key, 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tok, "BEGIN") || strings.Contains(tok, "PRIVATE") {
		t.Errorf("JWT string looks like it contains key material: %s", tok)
	}
}

// ----------------------------------------------------------------------
// Request decoding and authorisation tests
// ----------------------------------------------------------------------

func TestDecodeRequest_OK(t *testing.T) {
	body := `{"repo":"simons-agent-space/agentctl","profile":"builder"}`
	repo, prof, verr := decodeRequest(strings.NewReader(body))
	if verr != nil {
		t.Fatalf("decodeRequest: %v", verr)
	}
	if repo != "simons-agent-space/agentctl" {
		t.Errorf("repo = %q", repo)
	}
	if prof != ProfileBuilder {
		t.Errorf("profile = %q", prof)
	}
}

func TestDecodeRequest_BadJSON(t *testing.T) {
	_, _, verr := decodeRequest(strings.NewReader("not json"))
	if verr == nil {
		t.Errorf("expected error for bad JSON")
	}
}

func TestDecodeRequest_MissingRepo(t *testing.T) {
	body := `{"profile":"builder"}`
	_, _, verr := decodeRequest(strings.NewReader(body))
	if verr == nil || verr.Code != "BAD_REQUEST" {
		t.Errorf("expected BAD_REQUEST, got %v", verr)
	}
}

func TestDecodeRequest_MissingProfile(t *testing.T) {
	body := `{"repo":"x/y"}`
	_, _, verr := decodeRequest(strings.NewReader(body))
	if verr == nil || verr.Code != "BAD_REQUEST" {
		t.Errorf("expected BAD_REQUEST, got %v", verr)
	}
}

func TestDecodeRequest_UnknownField(t *testing.T) {
	body := `{"repo":"x/y","profile":"builder","sneaky":"value"}`
	_, _, verr := decodeRequest(strings.NewReader(body))
	if verr == nil {
		t.Errorf("expected error for unknown field")
	}
}

func TestAuthorize_OK(t *testing.T) {
	c := validConfig()
	name, perms, verr := c.authorize("simons-agent-space/agentctl", ProfileBuilder)
	if verr != nil {
		t.Errorf("authorize: %v", verr)
	}
	if name != "agentctl" {
		t.Errorf("name = %q, want agentctl", name)
	}
	if perms["contents"] != "write" {
		t.Errorf("perms[contents] = %q", perms["contents"])
	}
}

func TestAuthorize_NotBuilder(t *testing.T) {
	c := validConfig()
	_, _, verr := c.authorize("simons-agent-space/agentctl", "admin")
	if verr == nil || verr.Code != "PROFILE_NOT_ALLOWED" {
		t.Errorf("expected PROFILE_NOT_ALLOWED, got %v", verr)
	}
}

func TestAuthorize_RepoNotAllowed(t *testing.T) {
	c := validConfig()
	_, _, verr := c.authorize("simons-agent-space/other", ProfileBuilder)
	if verr == nil || verr.Code != "REPO_NOT_ALLOWED" {
		t.Errorf("expected REPO_NOT_ALLOWED, got %v", verr)
	}
}

func TestAuthorize_WrongOrg(t *testing.T) {
	c := validConfig()
	_, _, verr := c.authorize("other-org/agentctl", ProfileBuilder)
	if verr == nil || verr.Code != "REPO_NOT_ALLOWED" {
		t.Errorf("expected REPO_NOT_ALLOWED, got %v", verr)
	}
}

// ----------------------------------------------------------------------
// shared test helpers
// ----------------------------------------------------------------------

func writeTempKey(t *testing.T, data []byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "key-*.pem")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func decodeJWTForTest(t *testing.T, tok string) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(tok, ".")
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	plJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var hdr map[string]any
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		t.Fatal(err)
	}
	var pl map[string]any
	if err := json.Unmarshal(plJSON, &pl); err != nil {
		t.Fatal(err)
	}
	return hdr, pl
}

// ----------------------------------------------------------------------
// Cross-feature integration test
// ----------------------------------------------------------------------

func TestEndToEnd_MintsValidToken(t *testing.T) {
	const wantToken = "ghs_test_token"
	expires := time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	ghHandler := func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("missing Bearer prefix")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      wantToken,
			"expires_at": expires,
		})
	}
	srv, logBuf := makeTestServer(t, ghHandler)
	body := strings.NewReader(`{"repo":"simons-agent-space/agentctl","profile":"builder"}`)
	rec := httptest.NewRecorder()
	srv.handleToken(rec, httptest.NewRequest(http.MethodPost, "/token", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out response
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Token != wantToken {
		t.Errorf("token = %q, want %q", out.Token, wantToken)
	}
	if out.ExpiresAt == "" {
		t.Errorf("expires_at empty")
	}
	if !strings.Contains(logBuf.String(), `"op":"minted"`) {
		t.Errorf("minted event missing in log: %s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), wantToken) {
		t.Errorf("token leaked into audit log: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"repo_name":"agentctl"`) {
		t.Errorf("audit log should record the repo_name sent to GitHub: %s", logBuf.String())
	}
}
