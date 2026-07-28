// Package pusher renders the configured payload template and delivers the
// Vault token to the target API, optionally performing a pre-login request
// first (e.g. the CP4D /icp4d-api/v1/authorize session exchange).
package pusher

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"text/template"
	"time"

	"mint4v/internal/config"
)

const (
	maxErrorBodyBytes     = 256
	maxLoginResponseBytes = 1 << 20
)

type pushData struct {
	VaultToken string
	Accessor   string
	TTLSeconds int
	Extra      map[string]string
}

type loginData struct {
	Credentials map[string]string
}

// Pusher holds the parsed templates, credentials, and HTTP client for the
// target API. Construct with New; safe to reuse across pushes.
type Pusher struct {
	cfg       config.Push
	client    *http.Client
	bodyTmpl  *template.Template
	loginTmpl *template.Template
	creds     map[string]string
	logger    *slog.Logger
}

// New parses the templates and credential file referenced by cfg and builds
// the HTTP client, including the custom CA bundle if one is configured.
func New(cfg config.Push, logger *slog.Logger) (*Pusher, error) {
	p := &Pusher{cfg: cfg, logger: logger}

	var err error
	p.bodyTmpl, err = parseTemplate("push.body_template", cfg.BodyTemplate)
	if err != nil {
		return nil, err
	}

	if cfg.Login != nil {
		p.loginTmpl, err = parseTemplate("push.login.body_template", cfg.Login.BodyTemplate)
		if err != nil {
			return nil, err
		}
		if cfg.Login.CredentialsFile != "" {
			raw, err := os.ReadFile(cfg.Login.CredentialsFile)
			if err != nil {
				return nil, fmt.Errorf("push.login.credentials_file: %w", err)
			}
			if err := json.Unmarshal(raw, &p.creds); err != nil {
				return nil, fmt.Errorf("push.login.credentials_file: parsing JSON: %w", err)
			}
		}
	}

	// Redirects are refused outright: following one would turn the PATCH into
	// a bodyless GET whose 200 masks a failed delivery, or replay the token
	// body to whatever host the Location header names.
	p.client = &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return fmt.Errorf("refusing redirect to %s", req.URL.Redacted())
		},
	}
	if cfg.CACertFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("loading system cert pool: %w", err)
		}
		pem, err := os.ReadFile(cfg.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("push.ca_cert_file: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("push.ca_cert_file: no certificates found in %s", cfg.CACertFile)
		}
		// Clone the default transport rather than replacing it: a bare
		// &http.Transport{} would silently drop ProxyFromEnvironment and the
		// standard timeouts, making proxy behaviour depend on whether a CA is
		// configured.
		tp := http.DefaultTransport.(*http.Transport).Clone()
		tp.TLSClientConfig = &tls.Config{RootCAs: pool}
		p.client.Transport = tp
	}

	return p, nil
}

// Push delivers the token to the target API. It performs the pre-login
// exchange first when one is configured. The error text never contains the
// Vault token.
func (p *Pusher) Push(ctx context.Context, token, accessor string, ttlSeconds int) error {
	var bearer string
	if p.cfg.Login != nil {
		var err error
		bearer, err = p.preLogin(ctx)
		if err != nil {
			return fmt.Errorf("pre-login: %w", err)
		}
	}

	body, err := render(p.bodyTmpl, pushData{
		VaultToken: token,
		Accessor:   accessor,
		TTLSeconds: ttlSeconds,
		Extra:      p.cfg.Extra,
	})
	if err != nil {
		return fmt.Errorf("rendering payload template: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, p.cfg.Method, p.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range p.cfg.Headers {
		req.Header.Set(k, v)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("pushing to target: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("target returned %s: %s", resp.Status, describeErrorBody(resp))
	}
	p.logger.Info("pushed token to target", "url", p.cfg.URL, "status", resp.StatusCode, "accessor", accessor)
	return nil
}

func (p *Pusher) preLogin(ctx context.Context) (string, error) {
	body, err := render(p.loginTmpl, loginData{Credentials: p.creds})
	if err != nil {
		return "", fmt.Errorf("rendering login template: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.Login.URL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("login returned %s: %s", resp.Status, describeErrorBody(resp))
	}

	// The response size is capped (the client timeout bounds time, not bytes)
	// and must be exactly one JSON object — trailing data means the body is
	// not what the token-field lookup thinks it is.
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxLoginResponseBytes))
	var parsed map[string]any
	if err := dec.Decode(&parsed); err != nil {
		return "", fmt.Errorf("parsing login response: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", fmt.Errorf("login response has data after the JSON object")
	}
	bearer, err := lookupField(parsed, p.cfg.Login.TokenField)
	if err != nil {
		return "", err
	}
	return bearer, nil
}

// lookupField walks a dot-separated path (e.g. "token" or "data.token")
// through a decoded JSON object and returns the string at the leaf.
func lookupField(obj map[string]any, path string) (string, error) {
	parts := strings.Split(path, ".")
	var cur any = obj
	for _, part := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", fmt.Errorf("login response field %q: %q is not an object", path, part)
		}
		cur, ok = m[part]
		if !ok {
			return "", fmt.Errorf("login response has no field %q", path)
		}
	}
	s, ok := cur.(string)
	if !ok {
		return "", fmt.Errorf("login response field %q is not a string", path)
	}
	if s == "" {
		// An empty bearer would produce a push with no authentication at all.
		return "", fmt.Errorf("login response field %q is empty", path)
	}
	return s, nil
}

func parseTemplate(name, text string) (*template.Template, error) {
	// toJSON emits a value as a JSON literal, quotes included — the correct
	// way to substitute Extra or Credentials values that may contain quotes
	// or backslashes. printf "%q" is Go quoting, not JSON, and diverges on
	// some control and non-ASCII characters.
	funcs := template.FuncMap{
		"toJSON": func(v any) (string, error) {
			b, err := json.Marshal(v)
			return string(b), err
		},
	}
	tmpl, err := template.New(name).Funcs(funcs).Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return tmpl, nil
}

// render executes the template. Template errors can embed the data being
// rendered, so the returned error is the error string only, never the output.
func render(tmpl *template.Template, data any) ([]byte, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("template %s failed to execute", tmpl.Name())
	}
	return buf.Bytes(), nil
}

// describeErrorBody drains an error response and describes it without quoting
// it: target error bodies can echo the request payload, which carries the
// Vault token or the login credentials.
func describeErrorBody(resp *http.Response) string {
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
	return fmt.Sprintf("%d-byte %s body withheld", n, resp.Header.Get("Content-Type"))
}
