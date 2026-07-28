package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mint4v/internal/config"
)

// testPKI is a throwaway CA plus a server and a client certificate signed by
// it, written to files the way the chart mounts them.
type testPKI struct {
	caFile, clientCertFile, clientKeyFile string
	serverCert                            tls.Certificate
	caPool                                *x509.CertPool
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	dir := t.TempDir()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	issue := func(cn string, usage x509.ExtKeyUsage) ([]byte, *ecdsa.PrivateKey) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()),
			Subject:      pkix.Name{CommonName: cn},
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
			IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return der, key
	}
	writeFile := func(name string, block *pem.Block) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	serverDER, serverKey := issue("vault", x509.ExtKeyUsageServerAuth)
	clientDER, clientKey := issue("mint4v", x509.ExtKeyUsageClientAuth)
	clientKeyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return testPKI{
		caFile:         writeFile("ca.pem", &pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		clientCertFile: writeFile("client.crt", &pem.Block{Type: "CERTIFICATE", Bytes: clientDER}),
		clientKeyFile:  writeFile("client.key", &pem.Block{Type: "EC PRIVATE KEY", Bytes: clientKeyDER}),
		serverCert: tls.Certificate{
			Certificate: [][]byte{serverDER},
			PrivateKey:  serverKey,
		},
		caPool: pool,
	}
}

// A Vault listener with tls_require_and_verify_client_cert must accept the
// configured client_cert/client_key pair, reject a client without one, and
// reject a client relying on VAULT_CLIENT_CERT/KEY env (the config file is
// authoritative; the env path is deliberately dead).
func TestNewVaultClientMTLS(t *testing.T) {
	pki := newTestPKI(t)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"initialized":true,"sealed":false,"standby":false}`))
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{pki.serverCert},
		ClientCAs:    pki.caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	srv.StartTLS()
	defer srv.Close()

	health := func(cfg config.Vault) error {
		cfg.Address = srv.URL
		cfg.CACert = pki.caFile
		client, err := newVaultClient(cfg)
		if err != nil {
			return err
		}
		_, err = client.Sys().Health()
		return err
	}

	if err := health(config.Vault{ClientCert: pki.clientCertFile, ClientKey: pki.clientKeyFile}); err != nil {
		t.Errorf("mTLS with configured client cert: %v", err)
	}
	if err := health(config.Vault{}); err == nil {
		t.Error("a client-cert-requiring listener accepted a connection without one")
	}

	t.Setenv("VAULT_CLIENT_CERT", pki.clientCertFile)
	t.Setenv("VAULT_CLIENT_KEY", pki.clientKeyFile)
	if err := health(config.Vault{}); err == nil {
		t.Error("VAULT_CLIENT_CERT/KEY env must be ignored; the config file is authoritative")
	}
}
