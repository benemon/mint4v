// Package config loads and validates the minter's HCL configuration file.
package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
)

const defaultServiceAccountTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// Config is the root of the HCL configuration file.
type Config struct {
	LogLevel      string `hcl:"log_level,optional"`
	HealthAddress string `hcl:"health_address,optional"`
	Vault         Vault  `hcl:"vault,block"`
	Push          Push   `hcl:"push,block"`
}

// Vault configures the connection to Vault and the token lifecycle.
type Vault struct {
	Address     string `hcl:"address"`
	Namespace   string `hcl:"namespace,optional"`
	CACertFile  string `hcl:"ca_cert_file,optional"`
	RevokeGrace string `hcl:"revoke_grace,optional"`
	Auth        Auth   `hcl:"auth,block"`

	revokeGrace time.Duration
}

// RevokeGraceDuration is the parsed revoke_grace value.
func (v *Vault) RevokeGraceDuration() time.Duration { return v.revokeGrace }

// Auth selects and configures the Vault auth method used to log in.
type Auth struct {
	Method    string `hcl:"method"`
	Role      string `hcl:"role"`
	MountPath string `hcl:"mount_path,optional"`
	TokenFile string `hcl:"token_file,optional"`
}

// Push configures the request that delivers the Vault token to the target API.
// Templates are inline (HCL heredocs): Go templates interpolate with {{ }},
// HCL with ${ }, so the two never collide.
type Push struct {
	URL          string            `hcl:"url"`
	Method       string            `hcl:"method,optional"`
	CACertFile   string            `hcl:"ca_cert_file,optional"`
	BodyTemplate string            `hcl:"body_template"`
	Headers      map[string]string `hcl:"headers,optional"`
	Extra        map[string]string `hcl:"extra,optional"`
	Login        *Login            `hcl:"login,block"`
}

// Login is an optional pre-login request that exchanges a credential for a
// bearer token used on the push request (e.g. CP4D /icp4d-api/v1/authorize).
type Login struct {
	URL             string `hcl:"url"`
	BodyTemplate    string `hcl:"body_template"`
	TokenField      string `hcl:"token_field"`
	CredentialsFile string `hcl:"credentials_file,optional"`
}

// Load reads, decodes, and validates the HCL config file at path.
func Load(path string) (*Config, error) {
	var cfg Config
	if err := hclsimple.DecodeFile(path, nil, &cfg); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", path, err)
	}

	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.HealthAddress == "" {
		cfg.HealthAddress = ":8080"
	}
	if cfg.Vault.RevokeGrace == "" {
		cfg.Vault.RevokeGrace = "30s"
	}
	grace, err := time.ParseDuration(cfg.Vault.RevokeGrace)
	if err != nil {
		return nil, fmt.Errorf("vault.revoke_grace: %w", err)
	}
	cfg.Vault.revokeGrace = grace

	switch cfg.Vault.Auth.Method {
	case "kubernetes", "jwt":
	default:
		return nil, fmt.Errorf("vault.auth.method must be %q or %q, got %q", "kubernetes", "jwt", cfg.Vault.Auth.Method)
	}
	if cfg.Vault.Auth.MountPath == "" {
		cfg.Vault.Auth.MountPath = cfg.Vault.Auth.Method
	}
	if cfg.Vault.Auth.TokenFile == "" {
		cfg.Vault.Auth.TokenFile = defaultServiceAccountTokenFile
	}

	if cfg.Push.Method == "" {
		cfg.Push.Method = "POST"
	}
	cfg.Push.Method = strings.ToUpper(cfg.Push.Method)

	for name, raw := range map[string]string{
		"vault.address": cfg.Vault.Address,
		"push.url":      cfg.Push.URL,
	} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("%s: %q is not a valid http(s) URL", name, raw)
		}
	}
	return &cfg, nil
}
