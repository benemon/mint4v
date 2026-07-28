// Package config loads and validates the mint4v HCL configuration file.
//
// The schema is three blocks naming the three things involved: vault (where
// tokens come from), credentials (what authenticates to the target), and
// target (where tokens go). Block and attribute names follow Vault Agent
// vocabulary (ca_cert, token_path, labelled auth method blocks) so the
// config reads familiarly to Vault operators; see docs/config-design.md for
// the rationale and the deliberate divergences.
package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
)

const defaultServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// Config is the root of the HCL configuration file.
type Config struct {
	LogLevel      string       `hcl:"log_level,optional"`
	HealthAddress string       `hcl:"health_address,optional"`
	Vault         Vault        `hcl:"vault,block"`
	Credentials   *Credentials `hcl:"credentials,block"`
	Target        Target       `hcl:"target,block"`
}

// Vault configures the connection to Vault and the token lifecycle.
type Vault struct {
	Address     string `hcl:"address"`
	Namespace   string `hcl:"namespace,optional"`
	CACert      string `hcl:"ca_cert,optional"`
	ClientCert  string `hcl:"client_cert,optional"`
	ClientKey   string `hcl:"client_key,optional"`
	RevokeGrace string `hcl:"revoke_grace,optional"`
	Auth        Auth   `hcl:"auth,block"`

	revokeGrace time.Duration
}

// RevokeGraceDuration is the parsed revoke_grace value.
func (v *Vault) RevokeGraceDuration() time.Duration { return v.revokeGrace }

// Auth configures the Vault auth method used to log in. The block label
// selects the method: auth "kubernetes" { ... } or auth "jwt" { ... }.
type Auth struct {
	Method    string `hcl:"method,label"`
	Role      string `hcl:"role"`
	MountPath string `hcl:"mount_path,optional"`
	TokenPath string `hcl:"token_path,optional"`
}

// Credentials names the file whose flat JSON object backs the
// {{ .Credentials.* }} template data, consumed by both target header
// templates and the login body template.
type Credentials struct {
	File string `hcl:"file"`
}

// Target configures the request that delivers the Vault token to the target
// API. Templates are inline (HCL heredocs): Go templates interpolate with
// {{ }}, HCL with ${ }, so the two never collide.
type Target struct {
	URL          string            `hcl:"url"`
	Method       string            `hcl:"method,optional"`
	CACert       string            `hcl:"ca_cert,optional"`
	BodyTemplate string            `hcl:"body_template"`
	Headers      map[string]string `hcl:"headers,optional"`
	Extra        map[string]string `hcl:"extra,optional"`
	Login        *Login            `hcl:"login,block"`
}

// Login is an optional pre-login request that exchanges a credential for a
// bearer token used on the push request (e.g. CP4D /icp4d-api/v1/authorize).
type Login struct {
	URL          string `hcl:"url"`
	BodyTemplate string `hcl:"body_template"`
	TokenField   string `hcl:"token_field"`
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

	if (cfg.Vault.ClientCert == "") != (cfg.Vault.ClientKey == "") {
		return nil, fmt.Errorf("vault.client_cert and vault.client_key must be set together")
	}

	switch cfg.Vault.Auth.Method {
	case "kubernetes", "jwt":
	default:
		return nil, fmt.Errorf("vault.auth block label must be %q or %q, got %q", "kubernetes", "jwt", cfg.Vault.Auth.Method)
	}
	if cfg.Vault.Auth.MountPath == "" {
		cfg.Vault.Auth.MountPath = cfg.Vault.Auth.Method
	}
	if cfg.Vault.Auth.TokenPath == "" {
		cfg.Vault.Auth.TokenPath = defaultServiceAccountTokenPath
	}

	if cfg.Target.Method == "" {
		cfg.Target.Method = "POST"
	}
	cfg.Target.Method = strings.ToUpper(cfg.Target.Method)

	for name, raw := range map[string]string{
		"vault.address": cfg.Vault.Address,
		"target.url":    cfg.Target.URL,
	} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("%s: %q is not a valid http(s) URL", name, raw)
		}
	}
	return &cfg, nil
}

// CredentialsFile returns the configured credentials file path, or "" when
// no credentials block is present.
func (c *Config) CredentialsFile() string {
	if c.Credentials == nil {
		return ""
	}
	return c.Credentials.File
}
