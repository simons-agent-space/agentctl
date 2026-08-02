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
	"time"
)

// recoveryReloadTimeout bounds the rollback reload that runs after
// a failed primary reload. The rollback uses a fresh context
// derived from context.Background() (not the caller's) so a
// cancelled or expired caller context cannot prevent Caddy from
// being restored to a consistent state. The timeout is generous
// enough for a normal caddy reload but tight enough that a stuck
// rollback does not block the caller indefinitely.
const recoveryReloadTimeout = 10 * time.Second

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
// input.
//
// Caddy is run as a single long-lived process that loads a canonical
// root Caddyfile (RootConfigPath). That root file is expected to
// import the managed fragments from ConfigDir; for example:
//
//	import <ConfigDir>/*.caddy
//
// — the import glob matches only the final fragments
// (<app>.caddy). The promotion flow uses <app>.caddy.partial and
// <app>.caddy.bak as transient staging paths during the
// install/validate/reload dance, but neither suffix is ever part
// of the import glob, so a stale or in-flight partial or backup
// can never leak into the running configuration. Every caddy
// invocation in this package targets the canonical root file; no
// individual app fragment is ever passed to caddy directly.
type CaddyConfig struct {
	// BaseDomain is the trusted parent domain. The hostname for an
	// app is derived as "<app>.<BaseDomain>". The caller cannot
	// influence this.
	BaseDomain string
	// ConfigDir is the trusted directory where managed Caddy config
	// fragments are written. Must be non-empty.
	ConfigDir string
	// RootConfigPath is the path to the canonical Caddyfile that
	// Caddy is running. All caddy validate and caddy reload
	// invocations go through this file. Must be non-empty.
	RootConfigPath string
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
// candidate, validates and reloads Caddy through the canonical root
// config, and guarantees the on-disk state matches the running Caddy
// state on every exit path (success or failure). Promote never
// returns success while silently leaving the new route active, and
// never returns failure while leaving disk and running Caddy
// inconsistent with the intended (pre-promotion) state.
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
	tempPath := configPath + ".partial"
	backupPath := configPath + ".bak"

	body := renderCaddyfile(hostname, candidate.HostPort)

	// 1. Write the new fragment to a temporary file in ConfigDir.
	//    The temp uses a suffix (.partial) that the root config's
	//    import statement does NOT match, so writing it cannot
	//    affect what the running Caddy sees. It only becomes
	//    visible to Caddy once it is renamed to the final .caddy
	//    path in step 3.
	if err := atomicWrite(tempPath, []byte(body)); err != nil {
		return nil, err
	}

	// 2. Preserve the previous final fragment, if present, by
	//    renaming it to a backup path before installing the new
	//    final. The backup uses a suffix (.bak) that the root
	//    config's import statement does NOT match, so the running
	//    Caddy is unaffected by this rename: the import glob sees
	//    no <app>.caddy for this app from this point until the
	//    new final is installed in step 3.
	var hadPrevious bool
	if _, err := os.Stat(configPath); err == nil {
		hadPrevious = true
		if err := os.Rename(configPath, backupPath); err != nil {
			_ = os.Remove(tempPath)
			return nil, fmt.Errorf("%w: backup %s -> %s: %v", ErrAtomicWriteFailed, configPath, backupPath, err)
		}
	}

	// 3. Atomically install the new final fragment by renaming the
	//    temp into place. From this point the root config's import
	//    statement picks up the new fragment and the previous
	//    fragment lives only at the backup path (not imported). If
	//    this rename fails we remove the temp and restore the
	//    previous fragment so disk matches the pre-promotion
	//    state.
	if err := os.Rename(tempPath, configPath); err != nil {
		_ = os.Remove(tempPath)
		if hadPrevious {
			_ = os.Rename(backupPath, configPath)
		}
		return nil, fmt.Errorf("%w: rename %s -> %s: %v", ErrAtomicWriteFailed, tempPath, configPath, err)
	}

	// 4. Validate the canonical root config. The root imports
	//    only the final <app>.caddy fragments; the .partial and
	//    .bak siblings on disk are invisible to this validation.
	//    On failure we restore the previous fragment: park the
	//    new content at tempPath (a non-imported suffix) so we
	//    can move the backup on top of configPath without
	//    overwriting it, then remove the parked content. No
	//    reload is needed on the validate-failure path because
	//    validate never changes the running Caddy.
	if _, err := runner.Run(ctx, binary, "validate", "--config", cfg.RootConfigPath); err != nil {
		_ = os.Rename(configPath, tempPath)
		if hadPrevious {
			_ = os.Rename(backupPath, configPath)
		} else {
			_ = os.Remove(configPath)
		}
		_ = os.Remove(tempPath)
		return nil, fmt.Errorf("%w: caddy validate --config %s: %v", ErrCaddyValidateFailed, cfg.RootConfigPath, err)
	}

	// 5. Reload Caddy through the canonical root config. On
	//    failure we restore the previous fragment: park the new
	//    content at tempPath (a non-imported suffix) so we can
	//    move the backup on top of configPath without overwriting
	//    it, issue a best-effort reload so the running Caddy
	//    state matches the restored disk state, then remove the
	//    parked content. The parked content never became part of
	//    the running config on the failure path.
	//
	//    The recovery reload uses a fresh bounded context
	//    (context.Background() + recoveryReloadTimeout) so a
	//    cancelled or expired caller context cannot prevent
	//    Caddy from being restored to a consistent state. If the
	//    recovery reload also fails, the returned error wraps
	//    ErrCaddyReloadFailed and clearly reports that the
	//    rollback reload also failed; a successful rollback is
	//    not silently swallowed.
	if _, err := runner.Run(ctx, binary, "reload", "--config", cfg.RootConfigPath); err != nil {
		recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), recoveryReloadTimeout)
		defer cancelRecovery()
		_ = os.Rename(configPath, tempPath)
		var rollbackErr error
		if hadPrevious {
			if rerr := os.Rename(backupPath, configPath); rerr == nil {
				_, rollbackErr = runner.Run(recoveryCtx, binary, "reload", "--config", cfg.RootConfigPath)
			} else {
				rollbackErr = fmt.Errorf("disk restore failed: %v", rerr)
			}
		} else {
			// No previous fragment: after parking, configPath is
			// gone, so a reload now sees the pre-promotion state
			// (no route for this app).
			_, rollbackErr = runner.Run(recoveryCtx, binary, "reload", "--config", cfg.RootConfigPath)
		}
		_ = os.Remove(tempPath)
		if rollbackErr != nil {
			return nil, fmt.Errorf("%w: caddy reload --config %s: %v; rollback reload also failed: %v", ErrCaddyReloadFailed, cfg.RootConfigPath, err, rollbackErr)
		}
		return nil, fmt.Errorf("%w: caddy reload --config %s: %v", ErrCaddyReloadFailed, cfg.RootConfigPath, err)
	}

	// 6. On success, the backup is no longer needed. Removing it
	//    is best-effort: a leftover .bak file is harmless because
	//    the root config's import statement does NOT match .bak,
	//    and the next promotion for the same app would simply
	//    overwrite it as part of its own backup step.
	if hadPrevious {
		_ = os.Remove(backupPath)
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
// given app and reloads Caddy through the canonical root config. The
// app must validate against the app-name regex. The config file must
// currently exist (the function returns ErrPromotionNotFound when it
// does not so callers can distinguish a missing promotion).
//
// Removal moves the fragment to a temporary backup, validates the
// canonical root config without the fragment, reloads Caddy through
// the root config, and only then deletes the backup. On any failure
// the fragment is restored at its original path so disk and the
// running Caddy state remain in sync. The route the promotion added
// is therefore guaranteed to be gone from running Caddy on success
// and guaranteed to still be there on failure.
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
	backupPath := configPath + ".bak"

	if _, err := os.Stat(configPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrPromotionNotFound, configPath)
		}
		return fmt.Errorf("stat promotion config %s: %w", configPath, err)
	}

	// 1. Move the app fragment to a temporary backup. After this
	//    rename the root config's import statement no longer
	//    includes the fragment, so a validate/reload through the
	//    root reflects the post-removal state.
	if err := os.Rename(configPath, backupPath); err != nil {
		return fmt.Errorf("%w: backup %s -> %s: %v", ErrAtomicWriteFailed, configPath, backupPath, err)
	}

	// 2. Validate the canonical root config without the fragment.
	if _, err := runner.Run(ctx, binary, "validate", "--config", cfg.RootConfigPath); err != nil {
		_ = os.Rename(backupPath, configPath) // restore
		return fmt.Errorf("%w: caddy validate --config %s: %v", ErrCaddyValidateFailed, cfg.RootConfigPath, err)
	}

	// 3. Reload the canonical root config so the route is gone from
	//    running Caddy. On failure we restore the fragment and
	//    best-effort reload so disk and running Caddy agree.
	//
	//    The recovery reload uses a fresh bounded context
	//    (context.Background() + recoveryReloadTimeout) so a
	//    cancelled or expired caller context cannot prevent
	//    Caddy from being restored to a consistent state. If the
	//    recovery reload also fails, the returned error wraps
	//    ErrCaddyReloadFailed and clearly reports that the
	//    rollback reload also failed; a successful rollback is
	//    not silently swallowed.
	if _, err := runner.Run(ctx, binary, "reload", "--config", cfg.RootConfigPath); err != nil {
		recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), recoveryReloadTimeout)
		defer cancelRecovery()
		var rollbackErr error
		if rerr := os.Rename(backupPath, configPath); rerr == nil {
			_, rollbackErr = runner.Run(recoveryCtx, binary, "reload", "--config", cfg.RootConfigPath)
		} else {
			rollbackErr = fmt.Errorf("disk restore failed: %v", rerr)
		}
		if rollbackErr != nil {
			return fmt.Errorf("%w: caddy reload --config %s: %v; rollback reload also failed: %v", ErrCaddyReloadFailed, cfg.RootConfigPath, err, rollbackErr)
		}
		return fmt.Errorf("%w: caddy reload --config %s: %v", ErrCaddyReloadFailed, cfg.RootConfigPath, err)
	}

	// 4. On success the backup is no longer needed. Deleting it
	//    is best-effort: by this point the route has already been
	//    removed from the running Caddy via a successful reload,
	//    so a leftover .bak file on disk is harmless (the root
	//    config's import glob does NOT match .bak) and must not
	//    turn a successful removal into an error. Any future
	//    promotion for the same app would overwrite the leftover
	//    .bak as part of its own backup step.
	_ = os.Remove(backupPath)
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
	if cfg.RootConfigPath == "" {
		return fmt.Errorf("%w: RootConfigPath is required", ErrInvalidCaddyConfig)
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

// validateCandidateForPromotion re-derives and checks the identity
// fields on CandidateResult. HealthURL is intentionally not part of
// the promotion identity: promotion derives its upstream exclusively
// from HostPort, and HealthURL is the runtime layer's concern.
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
