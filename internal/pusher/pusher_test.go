package pusher

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mint4v/internal/config"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPushRendersTemplateAndHeaders(t *testing.T) {
	var got struct {
		method, auth, contentType string
		body                      map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.auth = r.Header.Get("Authorization")
		got.contentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got.body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p, err := New(config.Push{
		URL:          srv.URL,
		Method:       "PUT",
		BodyTemplate: `{"secret":"{{ .VaultToken }}","accessor":"{{ .Accessor }}","ttl":{{ .TTLSeconds }},"id":"{{ .Extra.vault_id }}"}`,
		Headers:      map[string]string{"Content-Type": "application/json"},
		Extra:        map[string]string{"vault_id": "42"},
	}, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := p.Push(context.Background(), "hvs.secret", "acc-1", 900); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got.method != "PUT" || got.contentType != "application/json" || got.auth != "" {
		t.Errorf("request: method=%q content-type=%q auth=%q", got.method, got.contentType, got.auth)
	}
	want := map[string]any{"secret": "hvs.secret", "accessor": "acc-1", "ttl": float64(900), "id": "42"}
	for k, v := range want {
		if got.body[k] != v {
			t.Errorf("body[%q]: got %v, want %v", k, got.body[k], v)
		}
	}
}

func TestPushWithPreLogin(t *testing.T) {
	const sessionToken = "cpd-session-token"
	var pushedAuth string
	var loginBody map[string]string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /icp4d-api/v1/authorize", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&loginBody); err != nil {
			t.Errorf("login body is not JSON: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"token": sessionToken}})
	})
	mux.HandleFunc("POST /push", func(w http.ResponseWriter, r *http.Request) {
		pushedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p, err := New(config.Push{
		URL:          srv.URL + "/push",
		Method:       "POST",
		BodyTemplate: `{"secret":"{{ .VaultToken }}"}`,
		Login: &config.Login{
			URL:             srv.URL + "/icp4d-api/v1/authorize",
			BodyTemplate:    `{"username":"{{ .Credentials.username }}","api_key":"{{ .Credentials.api_key }}"}`,
			TokenField:      "data.token",
			CredentialsFile: writeFile(t, "credentials.json", `{"username":"admin","api_key":"k3y"}`),
		},
	}, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := p.Push(context.Background(), "hvs.secret", "acc-1", 900); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if loginBody["username"] != "admin" || loginBody["api_key"] != "k3y" {
		t.Errorf("login body: got %v", loginBody)
	}
	if pushedAuth != "Bearer "+sessionToken {
		t.Errorf("push Authorization: got %q", pushedAuth)
	}
}

func TestPushCustomCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// PEM-encode the test server's self-signed certificate as the custom CA.
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))

	tmpl := "{{ .VaultToken }}"

	// Without the CA the push must fail TLS verification.
	noCA, err := New(config.Push{URL: srv.URL, Method: "POST", BodyTemplate: tmpl}, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := noCA.Push(context.Background(), "tok", "acc", 60); err == nil {
		t.Fatal("push without custom CA should fail TLS verification")
	}

	withCA, err := New(config.Push{
		URL:          srv.URL,
		Method:       "POST",
		BodyTemplate: tmpl,
		CACertFile:   writeFile(t, "ca.pem", caPEM),
	}, discard())
	if err != nil {
		t.Fatalf("New with CA: %v", err)
	}
	if err := withCA.Push(context.Background(), "tok", "acc", 60); err != nil {
		t.Errorf("push with custom CA: %v", err)
	}
}

func TestPushErrorNeverContainsToken(t *testing.T) {
	const token = "hvs.supersecret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	p, err := New(config.Push{
		URL:          srv.URL,
		Method:       "POST",
		BodyTemplate: `{"secret":"{{ .VaultToken }}","missing":"{{ .Extra.nope }}"}`,
	}, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Template failure path: missingkey=error triggers, and the error must
	// not leak the token that was being rendered.
	err = p.Push(context.Background(), token, "acc", 60)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("template error leaks token: %v", err)
	}

	// HTTP failure path.
	p2, err := New(config.Push{
		URL:          srv.URL,
		Method:       "POST",
		BodyTemplate: `{{ .VaultToken }}`,
	}, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = p2.Push(context.Background(), token, "acc", 60)
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("want 400 error, got %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("http error leaks token: %v", err)
	}
}

func TestLookupField(t *testing.T) {
	obj := map[string]any{"token": "a", "data": map[string]any{"token": "b", "n": float64(1)}}
	for _, tc := range []struct {
		path, want, wantErr string
	}{
		{path: "token", want: "a"},
		{path: "data.token", want: "b"},
		{path: "data.n", wantErr: "not a string"},
		{path: "data.absent", wantErr: "no field"},
		{path: "token.deeper", wantErr: "not an object"},
	} {
		got, err := lookupField(obj, tc.path)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("lookupField(%q): want error containing %q, got %v", tc.path, tc.wantErr, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("lookupField(%q): got %q, %v", tc.path, got, err)
		}
	}
}
