// Package vaulttoken logs in to Vault with a Kubernetes ServiceAccount token,
// keeps the resulting Vault token renewed, rotates it when renewal is
// exhausted, and revokes it on shutdown. The token only ever lives in memory;
// log lines carry the accessor, never the token.
package vaulttoken

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/hashicorp/vault/api"
	k8sauth "github.com/hashicorp/vault/api/auth/kubernetes"

	"mint4v/internal/config"
)

const (
	revokeTimeout = 10 * time.Second
	maxBackoff    = 30 * time.Second
)

// errNonRenewable marks a role-shape misconfiguration: retrying the login
// would mint another orphaned token, so callers must fail fast instead.
var errNonRenewable = errors.New("vault returned a non-renewable token; " +
	"the auth role must issue renewable service tokens (token_type=service, not batch)")

// PushFunc delivers a freshly minted token to the target API.
type PushFunc func(ctx context.Context, token, accessor string, ttlSeconds int) error

// Manager owns the Vault token lifecycle: login, push, renew, rotate, revoke.
type Manager struct {
	client *api.Client
	auth   config.Auth
	grace  time.Duration
	push   PushFunc
	logger *slog.Logger

	mu          sync.Mutex
	tokenExpiry time.Time
	pushOK      bool
}

// NewManager wires a configured Vault client to a push function. grace is how
// long the previous token stays valid after its replacement has been pushed.
func NewManager(client *api.Client, auth config.Auth, grace time.Duration, push PushFunc, logger *slog.Logger) *Manager {
	return &Manager{client: client, auth: auth, grace: grace, push: push, logger: logger}
}

// Healthy reports whether the held token is unexpired and the last push
// succeeded. Suitable for a readiness probe only: an unreachable target makes
// this false while the retry loop is handling it, and a liveness restart in
// that state would revoke a still-valid token.
func (m *Manager) Healthy() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case !m.pushOK:
		return false, "token not yet pushed to target"
	case time.Now().After(m.tokenExpiry):
		return false, "vault token expired"
	default:
		return true, "ok"
	}
}

// Run blocks until ctx is cancelled, at which point the current token is
// revoked. The first login and push fail fast (returning an error) so a
// misconfigured pod surfaces as a crash loop; failures during rotation are
// retried while the previous token drains.
func (m *Manager) Run(ctx context.Context) error {
	secret, err := m.login(ctx)
	if err != nil {
		return fmt.Errorf("initial login: %w", err)
	}
	if err := m.pushSecret(ctx, secret); err != nil {
		m.revoke(secret)
		return fmt.Errorf("initial push: %w", err)
	}

	// Grace-period revocations of rotated-out tokens run concurrently so the
	// replacement is watched (and renewed) from the moment it is pushed; wait
	// for them so no revocation is abandoned by an exiting Run.
	var pending sync.WaitGroup
	defer pending.Wait()

	for {
		watcher, err := m.client.NewLifetimeWatcher(&api.LifetimeWatcherInput{Secret: secret})
		if err != nil {
			m.revoke(secret)
			return fmt.Errorf("creating lifetime watcher: %w", err)
		}
		go watcher.Start()

	watch:
		for {
			select {
			case err := <-watcher.DoneCh():
				if err != nil {
					m.logger.Warn("token renewal ended", "error", err)
				} else {
					m.logger.Info("token reached max TTL, rotating")
				}
				break watch
			case renewal := <-watcher.RenewCh():
				m.setExpiry(renewal.Secret.Auth.LeaseDuration)
				m.logger.Debug("token renewed", "accessor", renewal.Secret.Auth.Accessor, "ttl_seconds", renewal.Secret.Auth.LeaseDuration)
			case <-ctx.Done():
				watcher.Stop()
				m.revoke(secret)
				m.logger.Info("shut down, token revoked")
				return nil
			}
		}

		next, err := m.rotate(ctx)
		if err != nil {
			// Cancelled mid-rotation, or the role now issues non-renewable
			// tokens (fatal: surface as a crash rather than retry forever).
			m.revoke(secret)
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("rotation: %w", err)
		}

		// The replacement is live in the target; let the old token drain for
		// the grace period (cut short on shutdown) without blocking the loop,
		// which moves straight on to watching the replacement.
		old := secret
		secret = next
		pending.Add(1)
		go func() {
			defer pending.Done()
			select {
			case <-time.After(m.grace):
			case <-ctx.Done():
			}
			m.revoke(old)
		}()
	}
}

// rotate acquires and pushes a replacement token, retrying with backoff until
// it succeeds or ctx is cancelled.
func (m *Manager) rotate(ctx context.Context) (*api.Secret, error) {
	backoff := time.Second
	for {
		secret, err := m.login(ctx)
		if err == nil {
			if err = m.pushSecret(ctx, secret); err == nil {
				return secret, nil
			}
			m.revoke(secret)
		} else if errors.Is(err, errNonRenewable) {
			// Every retry would mint (and orphan, for batch tokens) another
			// token against the misconfigured role.
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		m.logger.Warn("rotation failed, retrying", "error", err, "backoff", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (m *Manager) login(ctx context.Context) (*api.Secret, error) {
	var secret *api.Secret
	var err error

	switch m.auth.Method {
	case "kubernetes":
		var auth *k8sauth.KubernetesAuth
		auth, err = k8sauth.NewKubernetesAuth(m.auth.Role,
			k8sauth.WithMountPath(m.auth.MountPath),
			k8sauth.WithServiceAccountTokenPath(m.auth.TokenPath))
		if err != nil {
			return nil, err
		}
		secret, err = m.client.Auth().Login(ctx, auth)
	case "jwt":
		var jwt []byte
		jwt, err = os.ReadFile(m.auth.TokenPath)
		if err != nil {
			return nil, fmt.Errorf("reading service account token: %w", err)
		}
		secret, err = m.client.Logical().WriteWithContext(ctx, "auth/"+m.auth.MountPath+"/login", map[string]any{
			"role": m.auth.Role,
			"jwt":  string(jwt),
		})
	}
	if err != nil {
		return nil, err
	}
	if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
		return nil, fmt.Errorf("login returned no auth data")
	}
	if m.auth.Method == "jwt" {
		// The kubernetes auth helper sets the client token itself; the raw
		// logical write does not.
		m.client.SetToken(secret.Auth.ClientToken)
	}
	if !secret.Auth.Renewable {
		// Vault has already issued this token: revoke it rather than orphan
		// it. Best-effort — a batch token cannot revoke itself and dies at
		// its TTL, which revoke() logs as a warning.
		m.revoke(secret)
		return nil, errNonRenewable
	}
	m.setExpiry(secret.Auth.LeaseDuration)
	m.logger.Info("logged in to vault", "method", m.auth.Method, "accessor", secret.Auth.Accessor, "ttl_seconds", secret.Auth.LeaseDuration)
	return secret, nil
}

func (m *Manager) pushSecret(ctx context.Context, secret *api.Secret) error {
	err := m.push(ctx, secret.Auth.ClientToken, secret.Auth.Accessor, secret.Auth.LeaseDuration)
	m.mu.Lock()
	m.pushOK = err == nil
	m.mu.Unlock()
	return err
}

// revoke revokes the given token using its own identity. It deliberately uses
// a fresh context so revocation still runs during shutdown.
func (m *Manager) revoke(secret *api.Secret) {
	ctx, cancel := context.WithTimeout(context.Background(), revokeTimeout)
	defer cancel()

	// CloneWithHeaders, not Clone: the Vault namespace travels in the
	// X-Vault-Namespace header and must survive into the revoking client.
	clone, err := m.client.CloneWithHeaders()
	if err == nil {
		clone.SetToken(secret.Auth.ClientToken)
		err = clone.Auth().Token().RevokeSelfWithContext(ctx, "")
	}
	if err != nil {
		m.logger.Warn("failed to revoke token; it will expire at its TTL", "accessor", secret.Auth.Accessor, "error", err)
		return
	}
	m.logger.Info("revoked token", "accessor", secret.Auth.Accessor)
}

func (m *Manager) setExpiry(ttlSeconds int) {
	m.mu.Lock()
	m.tokenExpiry = time.Now().Add(time.Duration(ttlSeconds) * time.Second)
	m.mu.Unlock()
}
