package gitbridge

import (
	"context"
	"crypto/rsa"
	"fmt"
	"time"

	"github.com/simons-agent-space/agentctl/internal/github"
	"github.com/simons-agent-space/agentctl/internal/policy"
)

// tokenIssuer mints installation tokens for a fixed GitHub App.
// It holds the RSA private key in memory only. The key is never copied,
// serialised, or logged. Callers must not print or otherwise expose the
// issuer; the broker owns it for the lifetime of the process.
type tokenIssuer struct {
	appID        int64
	installation int64
	privateKey   *rsa.PrivateKey
	githubClient *github.Client
}

// newTokenIssuer loads the private key once at startup. After this call
// returns, the key on disk is no longer needed: the broker has its own
// in-memory copy and the file can be unmounted.
func newTokenIssuer(cfg *Config, ghc *github.Client) (*tokenIssuer, error) {
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

// mintAndReturn fetches a fresh installation token for repoSlug using
// prof. It returns the token, its expiry, and any error. The JWT used
// to authenticate the request and the installation token returned by
// GitHub are both kept out of the audit log; the caller is responsible
// for redacting any further material before passing it on.
func (i *tokenIssuer) mintAndReturn(
	ctx context.Context,
	repoSlug string,
	prof policy.Profile,
) (string, time.Time, error) {
	jwt, err := mintJWT(i.privateKey, i.appID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("mint jwt: %w", err)
	}
	perms := make([]github.Permission, 0, len(prof.Permissions))
	for _, p := range prof.Permissions {
		perms = append(perms, github.Permission{Scope: p.Scope, Access: p.Access})
	}
	req := github.AccessTokenRequest{
		Repositories: []string{repoSlug},
		Permissions:  perms,
	}
	resp, err := i.githubClient.MintInstallationToken(ctx, jwt, i.installation, req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("mint installation token: %w", err)
	}
	return resp.Token, resp.ExpiresAt, nil
}
