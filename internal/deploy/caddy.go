package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Sentinel errors returned by the Caddy promotion layer.
var (
	ErrInvalidCaddyConfig   = errors.New("invalid Caddy configuration")
	ErrInvalidDomain        = errors.New("invalid base domain")
	ErrAtomicWriteFailed    = errors.New("atomic config write failed")
	ErrCaddyValidateFailed  = errors.New("caddy validate failed")
	ErrCaddyReloadFailed    = errors.New("caddy reload failed")
	ErrPromotionNotFound    = errors.New("promotion config not found")
	ErrPromotionStillExists = errors.New("promotion config still exists")
)

// hostPortRe matches a numeric port in [1, 65535].
var hostPortRe = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)

// domainLabelRe matches a single DNS label (letters, digits, hyphens,
// not starting or ending with a hyphen).
var domainLabelRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// CaddyConfig holds trusted host-side configuration for Caddy
// promotion. All fields are trusted host configuration, not caller
// input. The Caddy binary path defaults to "caddy" if empty.
type CaddyConfig struct {
	// BaseDomain is the trusted parent domain. The hostname for an
	// app is derived as "<app>.<BaseDomain>". The caller cannot
	// influence this.
	BaseDomain string
	// ConfigDir is the trusted directory where managed Caddy config
	// fragments are written. Must be non-empty.
	ConfigDir string
	// CaddyBinary is the path to the caddy binary. Defaults to
	// "caddy" when empty.
	CaddyBinary string
}

// PromotionResult is the outcome of a successful promotion.
type PromotionResult struct {
	App        string
	Commit     string
	Hostname   string
	Upstream   string
	ConfigPath string
}

// Promote writes a managed Caddy config fragment for the given
// candidate, validates it, and reloads Caddy. The previous config is
// left untouched if validation or reload fails.
func Promote(ctx context.Context, cfg CaddyConfig, candidate CandidateResult) (*PromotionResult, error) {
	return promote(ctx, cfg, candidate, caddyRunner{})
}

// caddyRunner is the production command runner for caddy invocations.
type caddyRunner struct{}

func (caddyRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func promote(ctx context.Context, cfg CaddyConfig, candidate CandidateResult, runner commandRunner) (*PromotionResult, error) {
	if err := validateCaddyConfig(cfg); err != nil {
		return nil, err
	}
	if err := validateCandidateForPromotion(candidate); err != nil {
		return nil, err
	}

	binary := cfg.CaddyBinary
	if binary == "" {
		binary = "caddy"
	}

	hostname := candidate.App + "." + cfg.BaseDomain
	upstream := fmt.Sprintf("127.0.0.1:%d", candidate.HostPort)
	configPath := filepath.Join(cfg.ConfigDir, candidate.App+".caddy")
	tempPath := configPath + ".tmp"

	body := renderCaddyfile(hostname, candidate.HostPort)

	if err := atomicWrite(tempPath, []byte(body)); err != nil {
		return nil, err
	}

	// Always try to remove the temp file on failure paths so we do not
	// leave stray .tmp fragments behind.
	cleanup := func() { _ = os.Remove(tempPath) }

	// 1. Validate before applying. If validation fails, the previous
	//    config (if any) at configPath is untouched.
	if _, err := runner.Run(ctx, binary, "validate", "--config", tempPath); err != nil {
		cleanup()
		return nil, fmt.Errorf("%w: caddy validate %s: %v", ErrCaddyValidateFailed, tempPath, err)
	}

	// 2. Reload Caddy with the new config. If reload fails, the
	//    previous config at configPath is untouched because we have
	//    not yet renamed temp -> final.
	if _, err := runner.Run(ctx, binary, "reload", "--config", tempPath); err != nil {
		cleanup()
		return nil, fmt.Errorf("%w: caddy reload --config %s: %v", ErrCaddyReloadFailed, tempPath, err)
	}

	// 3. Atomically replace the previous config with the new one.
	if err := os.Rename(tempPath, configPath); err != nil {
		cleanup()
		return nil, fmt.Errorf("%w: rename %s -> %s: %v", ErrAtomicWriteFailed, tempPath, configPath, err)
	}

	return &PromotionResult{
		App:        candidate.App,
		Commit:     candidate.Commit,
		Hostname:   hostname,
		Upstream:   upstream,
		ConfigPath: configPath,
	}, nil
}

// RemovePromotion removes the managed Caddy config fragment for the
// given app and reloads Caddy. The app must validate against the
// app-name regex. The config file must currently exist (the function
// is idempotent only if the caller considers a missing config a
// no-op; here we return ErrPromotionNotFound so callers can
// distinguish).
func RemovePromotion(ctx context.Context, cfg CaddyConfig, app string) error {
	return removePromotion(ctx, cfg, app, caddyRunner{})
}

func removePromotion(ctx context.Context, cfg CaddyConfig, app string, runner commandRunner) error {
	if err := validateCaddyConfig(cfg); err != nil {
		return err
	}
	if !appNameRe.MatchString(app) {
		return fmt.Errorf("%w: app %q does not match app-name format", ErrInvalidCandidate, app)
	}

	binary := cfg.CaddyBinary
	if binary == "" {
		binary = "caddy"
	}

	configPath := filepath.Join(cfg.ConfigDir, app+".caddy")

	if _, err := os.Stat(configPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrPromotionNotFound, configPath)
		}
		return fmt.Errorf("stat promotion config %s: %w", configPath, err)
	}

	if _, err := runner.Run(ctx, binary, "reload", "--config", configPath); err != nil {
		return fmt.Errorf("%w: caddy reload --config %s: %v", ErrCaddyReloadFailed, configPath, err)
	}

	if err := os.Remove(configPath); err != nil {
		return fmt.Errorf("remove promotion config %s: %w", configPath, err)
	}
	return nil
}

func validateCaddyConfig(cfg CaddyConfig) error {
	if cfg.BaseDomain == "" {
		return fmt.Errorf("%w: BaseDomain is required", ErrInvalidCaddyConfig)
	}
	if !isValidDomain(cfg.BaseDomain) {
		return fmt.Errorf("%w: BaseDomain %q is not a valid domain", ErrInvalidDomain, cfg.BaseDomain)
	}
	if cfg.ConfigDir == "" {
		return fmt.Errorf("%w: ConfigDir is required", ErrInvalidCaddyConfig)
	}
	return nil
}

func isValidDomain(d string) bool {
	if len(d) == 0 || len(d) > 253 {
		return false
	}
	// Reject anything with whitespace, control characters, or
	// characters that have special meaning in our derived hostname.
	if strings.ContainsAny(d, " \t\n\r\x00:") {
		return false
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !domainLabelRe.MatchString(l) {
			return false
		}
	}
	return true
}

func validateCandidateForPromotion(c CandidateResult) error {
	if !appNameRe.MatchString(c.App) {
		return fmt.Errorf("%w: app %q does not match app-name format", ErrInvalidCandidate, c.App)
	}
	if !shaRe.MatchString(c.Commit) {
		return fmt.Errorf("%w: commit %q is not exactly 40 lowercase hex characters", ErrInvalidCandidate, c.Commit)
	}
	expectedImage := deriveImage(c.App, c.Commit)
	if c.Image != expectedImage {
		return fmt.Errorf("%w: image %q does not match derived %q", ErrInvalidCandidate, c.Image, expectedImage)
	}
	expectedContainer := deriveContainerName(c.App, c.Commit)
	if c.ContainerName != expectedContainer {
		return fmt.Errorf("%w: container name %q does not match derived %q", ErrInvalidCandidate, c.ContainerName, expectedContainer)
	}
	if !hostPortRe.MatchString(strconv.Itoa(c.HostPort)) {
		return fmt.Errorf("%w: host port %d is not a valid port", ErrInvalidCandidate, c.HostPort)
	}
	if c.HealthURL == "" {
		return fmt.Errorf("%w: health URL is required", ErrInvalidCandidate)
	}
	return nil
}

func renderCaddyfile(hostname string, hostPort int) string {
	return fmt.Sprintf("%s {\n\treverse_proxy 127.0.0.1:%d\n}\n", hostname, hostPort)
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("%w: create dir %s: %v", ErrAtomicWriteFailed, dir, err)
	}
	tmp := path + ".write"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("%w: write temp %s: %v", ErrAtomicWriteFailed, tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%w: rename %s -> %s: %v", ErrAtomicWriteFailed, tmp, path, err)
	}
	return nil
}
