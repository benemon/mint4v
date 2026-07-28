package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadFull(t *testing.T) {
	cfg, err := Load("testdata/full.hcl")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.LogLevel != "debug" || cfg.HealthAddress != ":9090" {
		t.Errorf("top-level fields: got %q %q", cfg.LogLevel, cfg.HealthAddress)
	}
	if cfg.Vault.Namespace != "admin/infra" {
		t.Errorf("namespace: got %q", cfg.Vault.Namespace)
	}
	if got := cfg.Vault.RevokeGraceDuration(); got != 10*time.Second {
		t.Errorf("revoke grace: got %v", got)
	}
	if cfg.Vault.Auth.Method != "jwt" || cfg.Vault.Auth.MountPath != "jwt-ocp" || cfg.Vault.Auth.TokenPath != "/var/run/secrets/minter/token" {
		t.Errorf("auth: got %+v", cfg.Vault.Auth)
	}
	if cfg.CredentialsFile() != "/etc/minter/credentials/credentials.json" {
		t.Errorf("credentials file: got %q", cfg.CredentialsFile())
	}
	if cfg.Target.Method != "PUT" {
		t.Errorf("target method not uppercased: got %q", cfg.Target.Method)
	}
	if cfg.Target.Headers["Content-Type"] != "application/json" {
		t.Errorf("headers: got %v", cfg.Target.Headers)
	}
	if cfg.Target.Extra["vault_id"] != "1" {
		t.Errorf("extra: got %v", cfg.Target.Extra)
	}
	if cfg.Target.Login == nil || cfg.Target.Login.TokenField != "token" {
		t.Errorf("login: got %+v", cfg.Target.Login)
	}
}

func TestLoadMinimalDefaults(t *testing.T) {
	cfg, err := Load("testdata/minimal.hcl")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.LogLevel != "info" || cfg.HealthAddress != ":8080" {
		t.Errorf("defaults: got %q %q", cfg.LogLevel, cfg.HealthAddress)
	}
	if got := cfg.Vault.RevokeGraceDuration(); got != 30*time.Second {
		t.Errorf("default revoke grace: got %v", got)
	}
	if cfg.Vault.Auth.MountPath != "kubernetes" {
		t.Errorf("default mount path: got %q", cfg.Vault.Auth.MountPath)
	}
	if cfg.Vault.Auth.TokenPath != defaultServiceAccountTokenPath {
		t.Errorf("default token path: got %q", cfg.Vault.Auth.TokenPath)
	}
	if cfg.Target.Method != "POST" {
		t.Errorf("default target method: got %q", cfg.Target.Method)
	}
	if cfg.Target.Login != nil {
		t.Errorf("login should be nil, got %+v", cfg.Target.Login)
	}
	if cfg.CredentialsFile() != "" {
		t.Errorf("credentials file should default empty, got %q", cfg.CredentialsFile())
	}
}

func TestLoadErrors(t *testing.T) {
	base := `
vault {
  address = "%s"
  revoke_grace = "%s"
  auth "%s" {
    role = "cp4d"
  }
}
target {
  url           = "%s"
  body_template = "{{ .VaultToken }}"
}
`
	cases := []struct {
		name                                  string
		address, grace, method, pushURL, want string
	}{
		{"bad auth method", "http://v:8200", "30s", "approle", "http://t/api", "vault.auth block label"},
		{"bad revoke grace", "http://v:8200", "soon", "jwt", "http://t/api", "revoke_grace"},
		{"bad vault address", "not-a-url", "30s", "jwt", "http://t/api", "vault.address"},
		{"bad target url", "http://v:8200", "30s", "jwt", "ftp://t/api", "target.url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, base, tc.address, tc.grace, tc.method, tc.pushURL)
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLoadRejectsMissingRequiredAttr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.hcl")
	if err := os.WriteFile(path, []byte(`vault { address = "http://v:8200" auth "jwt" {} }`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("want error for missing required attributes, got nil")
	}
}

// The reference configurations in examples/ are documentation, but they must
// stay parseable against the real schema.
func TestExamplesParse(t *testing.T) {
	matches, err := filepath.Glob("../../examples/*.hcl")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no examples found; did examples/ move?")
	}
	for _, example := range matches {
		if _, err := Load(example); err != nil {
			t.Errorf("%s does not parse: %v", example, err)
		}
	}
}

func writeConfig(t *testing.T, format string, args ...any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.hcl")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(format, args...)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
