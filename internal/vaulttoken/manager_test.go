package vaulttoken

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/vault/api"

	"mint4v/internal/config"
)

// fakeVault implements just enough of the Vault HTTP API for the manager:
// login (kubernetes and jwt mounts), renew-self, and revoke-self. Tokens are
// issued with a 2s TTL and each may be renewed maxRenews times before
// renew-self starts failing, which forces the LifetimeWatcher to rotate.
type fakeVault struct {
	mu        sync.Mutex
	logins    int
	renews    map[string]int
	revoked   []string
	maxRenews int
	jwtSeen   string
}

func newFakeVault() *fakeVault {
	return &fakeVault{renews: map[string]int{}, maxRenews: 1}
}

func (f *fakeVault) handler() http.Handler {
	mux := http.NewServeMux()
	login := func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.logins++
		n := f.logins
		if strings.Contains(r.URL.Path, "/jwt/") {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.jwtSeen = body["jwt"]
		}
		f.mu.Unlock()
		writeAuth(w, fmt.Sprintf("tok-%d", n), fmt.Sprintf("acc-%d", n))
	}
	mux.HandleFunc("/v1/auth/kubernetes/login", login)
	mux.HandleFunc("/v1/auth/jwt/login", login)
	mux.HandleFunc("/v1/auth/token/renew-self", func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Vault-Token")
		f.mu.Lock()
		f.renews[token]++
		over := f.renews[token] > f.maxRenews
		f.mu.Unlock()
		if over {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"errors":["token is past its max TTL"]}`)
			return
		}
		writeAuth(w, token, "acc-renewed")
	})
	mux.HandleFunc("/v1/auth/token/revoke-self", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.revoked = append(f.revoked, r.Header.Get("X-Vault-Token"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func writeAuth(w http.ResponseWriter, token, accessor string) {
	json.NewEncoder(w).Encode(map[string]any{
		"auth": map[string]any{
			"client_token":   token,
			"accessor":       accessor,
			"renewable":      true,
			"lease_duration": 2,
		},
	})
}

func (f *fakeVault) revokedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revoked...)
}

type recordedPush struct {
	token, accessor string
}

type pushRecorder struct {
	mu     sync.Mutex
	pushes []recordedPush
	fail   int
}

func (r *pushRecorder) push(_ context.Context, token, accessor string, _ int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail > 0 {
		r.fail--
		return fmt.Errorf("target unavailable")
	}
	r.pushes = append(r.pushes, recordedPush{token, accessor})
	return nil
}

func (r *pushRecorder) tokens() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.pushes))
	for i, p := range r.pushes {
		out[i] = p.token
	}
	return out
}

func newTestManager(t *testing.T, fake *fakeVault, method string, push PushFunc) *Manager {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	vc := api.DefaultConfig()
	vc.Address = srv.URL
	client, err := api.NewClient(vc)
	if err != nil {
		t.Fatal(err)
	}

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("sa-jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	auth := config.Auth{Method: method, MountPath: method, Role: "cp4d", TokenFile: tokenFile}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewManager(client, auth, 50*time.Millisecond, push, logger)
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

func TestRunPushesRenewsRotatesAndRevokes(t *testing.T) {
	fake := newFakeVault()
	rec := &pushRecorder{}
	m := newTestManager(t, fake, "kubernetes", rec.push)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	// Initial login pushes tok-1; after maxRenews renewals the watcher gives
	// up and the manager rotates to tok-2, then revokes tok-1.
	waitFor(t, 15*time.Second, func() bool {
		toks := rec.tokens()
		return len(toks) >= 2 && toks[0] == "tok-1" && toks[1] == "tok-2"
	}, "rotation push of tok-2")

	waitFor(t, 5*time.Second, func() bool {
		revoked := fake.revokedTokens()
		for _, r := range revoked {
			if r == "tok-1" {
				return true
			}
		}
		return false
	}, "revocation of tok-1")

	if ok, reason := m.Healthy(); !ok {
		t.Errorf("manager should be healthy, got %q", reason)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	revoked := fake.revokedTokens()
	if last := revoked[len(revoked)-1]; last != "tok-2" {
		t.Errorf("current token not revoked on shutdown; revoked=%v", revoked)
	}
}

func TestRunJWTAuthSendsServiceAccountToken(t *testing.T) {
	fake := newFakeVault()
	rec := &pushRecorder{}
	m := newTestManager(t, fake, "jwt", rec.push)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	waitFor(t, 5*time.Second, func() bool { return len(rec.tokens()) >= 1 }, "initial push")
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.jwtSeen != "sa-jwt" {
		t.Errorf("jwt login sent %q, want the service account token", fake.jwtSeen)
	}
}

func TestRunInitialPushFailureRevokesAndExits(t *testing.T) {
	fake := newFakeVault()
	rec := &pushRecorder{fail: 100}
	m := newTestManager(t, fake, "kubernetes", rec.push)

	err := m.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "initial push") {
		t.Fatalf("want initial push error, got %v", err)
	}
	revoked := fake.revokedTokens()
	if len(revoked) != 1 || revoked[0] != "tok-1" {
		t.Errorf("token should be revoked after failed initial push, revoked=%v", revoked)
	}
	if ok, _ := m.Healthy(); ok {
		t.Error("manager should be unhealthy")
	}
}

func TestRotationRetriesPushFailure(t *testing.T) {
	fake := newFakeVault()
	rec := &pushRecorder{}
	m := newTestManager(t, fake, "kubernetes", rec.push)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	waitFor(t, 5*time.Second, func() bool { return len(rec.tokens()) == 1 }, "initial push")

	// Fail the next push attempt: the rotation must retry with a fresh
	// login and eventually deliver a later token.
	rec.mu.Lock()
	rec.fail = 1
	rec.mu.Unlock()

	waitFor(t, 20*time.Second, func() bool { return len(rec.tokens()) >= 2 }, "rotation push after retry")

	toks := rec.tokens()
	// The failed attempt consumed tok-2, so the successful retry pushes tok-3.
	if toks[1] != "tok-3" {
		t.Errorf("expected retried rotation to push tok-3, got %v", toks)
	}
	revoked := fake.revokedTokens()
	found := false
	for _, r := range revoked {
		if r == "tok-2" {
			found = true
		}
	}
	if !found {
		t.Errorf("token from failed push attempt should be revoked, revoked=%v", revoked)
	}

	cancel()
	<-done
}

func TestLoginRejectsNonRenewableToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{"client_token": "b.batch", "accessor": "acc", "renewable": false, "lease_duration": 60},
		})
	}))
	defer srv.Close()

	vc := api.DefaultConfig()
	vc.Address = srv.URL
	client, err := api.NewClient(vc)
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("sa-jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(client, config.Auth{Method: "jwt", MountPath: "jwt", Role: "r", TokenFile: tokenFile},
		time.Second, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := m.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "non-renewable") {
		t.Fatalf("want non-renewable token error, got %v", err)
	}
}
