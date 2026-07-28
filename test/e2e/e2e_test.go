//nolint:staticcheck // dot-imports are the ginkgo convention
package e2e

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/vault/api"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"mint4v/test/utils"
)

const (
	releaseName = "mint4v"
	// The ServiceAccount the chart creates (fullname == release name).
	saName = releaseName

	mockUsername = "cpdadmin"
	mockAPIKey   = "e2e-api-key"

	// Deliberately short so renewal and rotation happen inside the test run:
	// tokens live 1m and hit max TTL (forcing rotation) at 2m.
	tokenTTL    = "1m"
	tokenMaxTTL = "2m"

	vaultLocalPort = 18200
	mockLocalPort  = 18081
)

var (
	namespace = envOr("E2E_NAMESPACE", "mint4v-e2e")

	// vaultLicense switches the KIND dev Vault to Vault Enterprise, which
	// enables the namespace specs. Without it the OSS image is used and
	// those specs skip.
	vaultLicense = os.Getenv("VAULT_LICENSE")

	// vaultClient performs admin operations (mount config, token lookups)
	// from the test process: against the port-forwarded dev Vault in KIND
	// mode, or the external Vault in external mode.
	vaultClient *api.Client

	// activeVaultClient is the client tokenLookup uses; the Enterprise
	// namespace context repoints it at a namespaced clone.
	activeVaultClient *api.Client

	// deployVaultNamespace, when set, adds a namespace attribute to the
	// minter's vault block (used by the Enterprise namespace context).
	deployVaultNamespace string

	// caPEM and reviewerJWT are captured in BeforeAll for reuse by later
	// contexts that configure additional mounts.
	caPEM       string
	reviewerJWT string
)

// mountPath prefixes e2e auth mounts in external mode so the suite never
// collides with (or tears down) pre-existing mounts in a shared Vault
// namespace.
func mountPath(name string) string {
	if external {
		return "mint4v-e2e-" + name
	}
	return name
}

// vaultAddrForCluster is the Vault address pods inside the cluster use.
func vaultAddrForCluster() string {
	if external {
		return os.Getenv("E2E_VAULT_ADDR")
	}
	return fmt.Sprintf("http://vault.%s.svc:8200", namespace)
}

// kubernetesHost is the API server URL Vault uses for TokenReview.
func kubernetesHost() string {
	if external {
		return os.Getenv("E2E_KUBERNETES_HOST")
	}
	return "https://kubernetes.default.svc"
}

func newVaultAdminClient() *api.Client {
	vc := api.DefaultConfig()
	if external {
		vc.Address = os.Getenv("E2E_VAULT_ADDR")
	} else {
		vc.Address = fmt.Sprintf("http://127.0.0.1:%d", vaultLocalPort)
	}
	client, err := api.NewClient(vc)
	Expect(err).NotTo(HaveOccurred())
	if external {
		client.SetToken(os.Getenv("E2E_VAULT_TOKEN"))
		if ns := os.Getenv("E2E_VAULT_NAMESPACE"); ns != "" {
			client.SetNamespace(ns)
		}
	} else {
		client.SetToken("e2e-root")
	}
	return client
}

func enableAuth(path, authType string) {
	err := vaultClient.Sys().EnableAuthWithOptions(path, &api.EnableAuthOptions{Type: authType})
	if err != nil && !strings.Contains(err.Error(), "already in use") {
		Expect(err).NotTo(HaveOccurred())
	}
}

func vaultWrite(path string, data map[string]any) {
	_, err := vaultClient.Logical().Write(path, data)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "vault write %s", path)
}

// tokenLookup resolves a token via the admin client and returns its creation
// path (e.g. auth/kubernetes/login); the error is non-nil once the token is
// revoked or expired.
func tokenLookup(token string) (string, error) {
	secret, err := activeVaultClient.Logical().Write("auth/token/lookup", map[string]any{"token": token})
	if err != nil {
		return "", err
	}
	path, _ := secret.Data["path"].(string)
	return path, nil
}

// tokenRevoked reports whether Vault positively states the token is invalid.
// A transport failure or server error is NOT proof of revocation — only a
// 400/403 lookup response (Vault's bad-token answers) counts.
func tokenRevoked(token string) bool {
	_, err := activeVaultClient.Logical().Write("auth/token/lookup", map[string]any{"token": token})
	if err == nil {
		return false
	}
	var respErr *api.ResponseError
	return errors.As(err, &respErr) &&
		(respErr.StatusCode == http.StatusBadRequest || respErr.StatusCode == http.StatusForbidden)
}

// clusterCA returns the CA bundle that validates the API server endpoint
// Vault will call for TokenReview.
func clusterCA() string {
	if external {
		if f := os.Getenv("E2E_KUBERNETES_CA_FILE"); f != "" {
			ca, err := os.ReadFile(f)
			Expect(err).NotTo(HaveOccurred())
			return string(ca)
		}
		out, err := utils.Run(exec.Command("kubectl", "config", "view", "--raw", "--minify",
			"-o", "jsonpath={.clusters[0].cluster.certificate-authority-data}"))
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(BeEmpty(), "kubeconfig has no CA data; set E2E_KUBERNETES_CA_FILE")
		ca, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
		Expect(err).NotTo(HaveOccurred())
		return string(ca)
	}
	out, err := utils.Run(exec.Command("kubectl", "get", "configmap", "kube-root-ca.crt",
		"-n", namespace, "-o", "jsonpath={.data.ca\\.crt}"))
	Expect(err).NotTo(HaveOccurred())
	return out
}

// clusterJWKSPubkeys fetches the cluster's ServiceAccount JWKS and converts
// the RSA keys to PEM, for jwt auth mounts that cannot reach the cluster's
// OIDC issuer (external Vault).
func clusterJWKSPubkeys() []string {
	out, err := utils.Run(exec.Command("kubectl", "get", "--raw", "/openid/v1/jwks"))
	Expect(err).NotTo(HaveOccurred())
	var jwks struct {
		Keys []struct {
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	Expect(json.Unmarshal([]byte(out), &jwks)).To(Succeed())

	var pems []string
	for _, key := range jwks.Keys {
		if key.Kty != "RSA" {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(key.N)
		Expect(err).NotTo(HaveOccurred())
		eBytes, err := base64.RawURLEncoding.DecodeString(key.E)
		Expect(err).NotTo(HaveOccurred())
		pub := rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(new(big.Int).SetBytes(eBytes).Int64())}
		der, err := x509.MarshalPKIXPublicKey(&pub)
		Expect(err).NotTo(HaveOccurred())
		pems = append(pems, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
	}
	Expect(pems).NotTo(BeEmpty(), "cluster JWKS contained no RSA keys")
	return pems
}

// mockClient polls mockcpd with an explicit timeout: a stalled port-forward
// must fail the poll, not block until the suite deadline.
var mockClient = &http.Client{Timeout: 10 * time.Second}

// lastPushedToken returns the Vault token most recently accepted by mockcpd,
// via the port-forward established in BeforeAll.
func lastPushedToken() (string, error) {
	resp, err := mockClient.Get(fmt.Sprintf("http://127.0.0.1:%d/last", mockLocalPort))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mock /last returned %s: %s", resp.Status, body)
	}
	var payload struct {
		Details struct {
			AccessToken string `json:"access_token"`
		} `json:"details"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("mock payload is not the expected JSON: %q", body)
	}
	if payload.Details.AccessToken == "" {
		return "", fmt.Errorf("mock payload has empty details.access_token: %q", body)
	}
	return payload.Details.AccessToken, nil
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// splitImage splits "repo[:port]/name:tag" on the last colon.
func splitImage(image string) (repo, tag string) {
	i := strings.LastIndex(image, ":")
	return image[:i], image[i+1:]
}

// minterValues renders the chart values for a given auth method and mount.
// chartExtras is appended verbatim as extra top-level values (e.g. to switch
// the token source).
func minterValues(method, mount, chartExtras string) string {
	repo, tag := splitImage(minterImage)
	pullPolicy := "Never"
	if external {
		// The external images are mutable :latest tags in a registry;
		// IfNotPresent would silently run a stale cached build on nodes that
		// have seen the tag before.
		pullPolicy = "Always"
	}

	vaultBlockExtra := ""
	valuesExtra := ""
	if deployVaultNamespace != "" {
		vaultBlockExtra += fmt.Sprintf("\n    namespace = %q", deployVaultNamespace)
	}
	if external {
		if ns := os.Getenv("E2E_VAULT_NAMESPACE"); ns != "" {
			vaultBlockExtra += fmt.Sprintf("\n    namespace = %q", ns)
		}
		if caFile := os.Getenv("VAULT_CACERT"); caFile != "" {
			ca, err := os.ReadFile(caFile)
			Expect(err).NotTo(HaveOccurred())
			vaultBlockExtra += "\n    ca_cert = \"/etc/minter/tls/vault/ca.crt\""
			valuesExtra += "vaultCA: |\n" + indent(string(ca), "  ") + "\n"
		}
	}

	return fmt.Sprintf(`
image:
  repository: %[7]s
  tag: %[8]s
  pullPolicy: %[9]s

credentials: '{"username":"%[5]s","api_key":"%[6]s"}'
%[10]s
%[3]s

config: |
  log_level = "debug"

  vault {
    address      = "%[11]s"
    revoke_grace = "5s"%[12]s

    auth "%[2]s" {
      mount_path = "%[4]s"
      role       = "mint4v"
      token_path = "/var/run/secrets/minter/token"
    }
  }

  credentials {
    file = "/etc/minter/credentials/credentials.json"
  }

  target {
    url    = "http://mockcpd.%[1]s.svc:8080/zen-data/v2/vaults/1000330999:e2e-vault?validate_and_save=true"
    method = "PATCH"

    headers = {
      "Content-Type" = "application/json"
    }

    extra = {
      vault_address = "%[11]s"
    }

    body_template = <<-EOT
      {"details":{"vault_address":"{{ .Extra.vault_address }}","access_token":"{{ .VaultToken }}"}}
    EOT

    login {
      url         = "http://mockcpd.%[1]s.svc:8080/icp4d-api/v1/authorize"
      token_field = "token"

      body_template = <<-EOT
        {"username":{{ toJSON .Credentials.username }},"api_key":{{ toJSON .Credentials.api_key }}}
      EOT
    }
  }
`, namespace, method, chartExtras, mount, mockUsername, mockAPIKey,
		repo, tag, pullPolicy, valuesExtra, vaultAddrForCluster(),
		indentHCL(vaultBlockExtra))
}

// indentHCL reindents the vault-block extras for the values config heredoc.
func indentHCL(s string) string {
	return strings.ReplaceAll(s, "\n", "\n  ")
}

func helmDeploy(method, mount, chartExtras string) {
	values := filepath.Join(GinkgoT().TempDir(), "values.yaml")
	ExpectWithOffset(1, os.WriteFile(values, []byte(minterValues(method, mount, chartExtras)), 0o600)).To(Succeed())
	_, err := utils.Run(exec.Command("helm", "upgrade", "--install", releaseName, "charts/mint4v",
		"-n", namespace, "-f", values, "--wait", "--timeout", "3m"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

var _ = Describe("mint4v", Ordered, func() {
	BeforeAll(func() {
		By("creating the test namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		if _, err := utils.Run(cmd); err != nil && !strings.Contains(err.Error(), "AlreadyExists") {
			Expect(err).NotTo(HaveOccurred())
		}

		if !external {
			// Vault Enterprise (rolling minor tag, so the suite always exercises
			// the latest 2.0 patch) when a license is available; namespace specs
			// run in CI. OSS otherwise.
			vaultImage := "hashicorp/vault:1.18"
			licenseEnv := ""
			if vaultLicense != "" {
				vaultImage = "hashicorp/vault-enterprise:2.0-ent"
				// The license arrives via stdin so it never appears in a
				// logged command line or the host process table.
				cmd := exec.Command("kubectl", "create", "secret", "generic", "vault-license",
					"-n", namespace, "--from-file=license=/dev/stdin")
				cmd.Stdin = strings.NewReader(vaultLicense)
				if _, err := utils.Run(cmd); err != nil && !strings.Contains(err.Error(), "AlreadyExists") {
					Expect(err).NotTo(HaveOccurred())
				}
				licenseEnv = `
    env:
    - name: VAULT_LICENSE
      valueFrom:
        secretKeyRef:
          name: vault-license
          key: license`
			}

			By("deploying a dev-mode Vault")
			Expect(utils.KubectlApplyStdin(fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: vault
  namespace: %[1]s
  labels:
    app: vault
spec:
  containers:
  - name: vault
    image: %[2]s
    args: ["server", "-dev", "-dev-root-token-id=e2e-root", "-dev-listen-address=0.0.0.0:8200"]%[3]s
    ports:
    - containerPort: 8200
---
apiVersion: v1
kind: Service
metadata:
  name: vault
  namespace: %[1]s
spec:
  selector:
    app: vault
  ports:
  - port: 8200
    targetPort: 8200
`, namespace, vaultImage, licenseEnv))).To(Succeed())
			_, err := utils.Run(exec.Command("kubectl", "wait", "--for=condition=Ready", "pod/vault",
				"-n", namespace, "--timeout=120s"))
			Expect(err).NotTo(HaveOccurred())

			// Pod Ready only means the container started; wait for the dev
			// server to actually serve before tunnelling to it.
			Eventually(func(g Gomega) {
				_, err := utils.Run(exec.Command("kubectl", "exec", "vault", "-n", namespace, "--",
					"wget", "-qO-", "-T", "2", "http://127.0.0.1:8200/v1/sys/health"))
				g.Expect(err).NotTo(HaveOccurred())
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("port-forwarding to the dev Vault")
			stopPF, err := utils.PortForward(namespace, "svc/vault", vaultLocalPort, 8200)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(stopPF)
		}

		vaultClient = newVaultAdminClient()
		activeVaultClient = vaultClient

		By("deploying the mock CP4D API")
		mockTLSSkipVerify := "false"
		if external {
			// The mock validates pushed tokens against the real Vault; it
			// has no CA bundle mounted, so skip verification in the mock.
			mockTLSSkipVerify = "true"
		}
		Expect(utils.KubectlApplyStdin(fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mockcpd
  namespace: %[1]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: mockcpd
  template:
    metadata:
      labels:
        app: mockcpd
    spec:
      containers:
      - name: mockcpd
        image: %[2]s
        imagePullPolicy: %[5]s
        env:
        - name: MOCK_USERNAME
          value: %[3]s
        - name: MOCK_APIKEY
          value: %[4]s
        - name: MOCK_TLS_SKIP_VERIFY
          value: "%[6]s"
        ports:
        - containerPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: mockcpd
  namespace: %[1]s
spec:
  selector:
    app: mockcpd
  ports:
  - port: 8080
    targetPort: 8080
`, namespace, mockImage, mockUsername, mockAPIKey,
			map[bool]string{true: "IfNotPresent", false: "Never"}[external], mockTLSSkipVerify))).To(Succeed())
		_, err := utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/mockcpd",
			"-n", namespace, "--timeout=180s"))
		Expect(err).NotTo(HaveOccurred())

		By("port-forwarding to the mock CP4D API")
		stopMockPF, err := utils.PortForward(namespace, "svc/mockcpd", mockLocalPort, 8080)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(stopMockPF)

		By("granting TokenReview access")
		Expect(utils.KubectlApplyStdin(fmt.Sprintf(`
apiVersion: v1
kind: ServiceAccount
metadata:
  name: vault-reviewer
  namespace: %[1]s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: mint4v-e2e-vault-reviewer
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:auth-delegator
subjects:
- kind: ServiceAccount
  name: vault-reviewer
  namespace: %[1]s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: mint4v-e2e-clientreview
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:auth-delegator
subjects:
- kind: ServiceAccount
  name: %[2]s
  namespace: %[1]s
`, namespace, saName))).To(Succeed())

		caPEM = clusterCA()
		roleData := func(withAudience bool) map[string]any {
			d := map[string]any{
				"bound_service_account_names":      saName,
				"bound_service_account_namespaces": namespace,
				"token_policies":                   "default",
				"token_ttl":                        tokenTTL,
				"token_max_ttl":                    tokenMaxTTL,
			}
			if withAudience {
				d["audience"] = "vault"
			}
			return d
		}

		By("configuring the kubernetes auth mount (reviewer JWT)")
		reviewerOut, err := utils.Run(exec.Command("kubectl", "create", "token", "vault-reviewer",
			"-n", namespace, "--duration=2h"))
		Expect(err).NotTo(HaveOccurred())
		reviewerJWT = strings.TrimSpace(reviewerOut)
		enableAuth(mountPath("kubernetes"), "kubernetes")
		vaultWrite("auth/"+mountPath("kubernetes")+"/config", map[string]any{
			"kubernetes_host":    kubernetesHost(),
			"kubernetes_ca_cert": caPEM,
			"token_reviewer_jwt": reviewerJWT,
		})
		vaultWrite("auth/"+mountPath("kubernetes")+"/role/mint4v", roleData(true))

		if !external {
			// Reviewer-less mount: with no token_reviewer_jwt, the plugin
			// reviews as VAULT'S OWN pod ServiceAccount — only possible
			// with an in-cluster Vault.
			By("configuring a reviewer-less kubernetes auth mount (Vault pod SA self-review)")
			Expect(utils.KubectlApplyStdin(fmt.Sprintf(`
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: mint4v-e2e-selfreview
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:auth-delegator
subjects:
- kind: ServiceAccount
  name: default
  namespace: %s
`, namespace))).To(Succeed())
			enableAuth("kubernetes-selfreview", "kubernetes")
			vaultWrite("auth/kubernetes-selfreview/config", map[string]any{
				"kubernetes_host":    kubernetesHost(),
				"kubernetes_ca_cert": caPEM,
			})
			vaultWrite("auth/kubernetes-selfreview/role/mint4v", roleData(false))
		}

		// External-Vault semantics: disable_local_ca_jwt=true and no
		// reviewer JWT, so the CLIENT's login JWT performs its own
		// TokenReview (auth-delegator CRB on the minter SA above).
		By("configuring an external-style kubernetes auth mount (client JWT self-review)")
		enableAuth(mountPath("kubernetes-external"), "kubernetes")
		vaultWrite("auth/"+mountPath("kubernetes-external")+"/config", map[string]any{
			"kubernetes_host":      kubernetesHost(),
			"kubernetes_ca_cert":   caPEM,
			"disable_local_ca_jwt": true,
		})
		vaultWrite("auth/"+mountPath("kubernetes-external")+"/role/mint4v", roleData(false))

		By("configuring the jwt auth mount")
		enableAuth(mountPath("jwt"), "jwt")
		if external {
			// The cluster's OIDC issuer is not reachable from an external
			// Vault, so validate signatures against the extracted JWKS.
			vaultWrite("auth/"+mountPath("jwt")+"/config", map[string]any{
				"jwt_validation_pubkeys": clusterJWKSPubkeys(),
			})
		} else {
			// Vault requires oidc_discovery_url to exactly match the
			// cluster's issuer, so read it from the discovery document.
			discovery, err := utils.Run(exec.Command("kubectl", "get", "--raw", "/.well-known/openid-configuration"))
			Expect(err).NotTo(HaveOccurred())
			var oidc struct {
				Issuer string `json:"issuer"`
			}
			Expect(json.Unmarshal([]byte(discovery), &oidc)).To(Succeed())
			Expect(oidc.Issuer).NotTo(BeEmpty())

			Expect(utils.KubectlApplyStdin(fmt.Sprintf(`
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: mint4v-e2e-oidc-discovery
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:service-account-issuer-discovery
subjects:
- kind: Group
  name: system:unauthenticated
  apiGroup: rbac.authorization.k8s.io
`))).To(Succeed())
			vaultWrite("auth/"+mountPath("jwt")+"/config", map[string]any{
				"oidc_discovery_url":    oidc.Issuer,
				"oidc_discovery_ca_pem": caPEM,
			})
		}
		vaultWrite("auth/"+mountPath("jwt")+"/role/mint4v", map[string]any{
			"role_type":       "jwt",
			"user_claim":      "sub",
			"bound_audiences": "vault",
			"bound_subject":   fmt.Sprintf("system:serviceaccount:%s:%s", namespace, saName),
			"token_policies":  "default",
			"token_ttl":       tokenTTL,
			"token_max_ttl":   tokenMaxTTL,
		})
	})

	AfterAll(func() {
		if os.Getenv("E2E_KEEP") == "true" {
			_, _ = fmt.Fprintf(GinkgoWriter,
				"E2E_KEEP=true: leaving namespace %s, the helm release, and the Vault mounts in place\n", namespace)
			return
		}
		_, _ = utils.Run(exec.Command("helm", "uninstall", releaseName, "-n", namespace, "--ignore-not-found"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding",
			"mint4v-e2e-vault-reviewer", "mint4v-e2e-oidc-discovery",
			"mint4v-e2e-selfreview", "mint4v-e2e-clientreview", "--ignore-not-found"))
		if external {
			for _, mount := range []string{"kubernetes", "kubernetes-external", "jwt"} {
				_ = vaultClient.Sys().DisableAuth(mountPath(mount))
			}
		}
		_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found", "--timeout=120s"))
	})

	Context("with kubernetes auth", func() {
		var firstToken string

		It("logs in, performs the CP4D-style pre-login, and pushes a valid token", func() {
			helmDeploy("kubernetes", mountPath("kubernetes"), "")

			var token string
			Eventually(func(g Gomega) {
				var err error
				token, err = lastPushedToken()
				g.Expect(err).NotTo(HaveOccurred())
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			path, err := tokenLookup(token)
			Expect(err).NotTo(HaveOccurred())
			Expect(path).To(ContainSubstring("auth/" + mountPath("kubernetes") + "/login"))
			firstToken = token
		})

		It("renews the token in place past its initial TTL without re-pushing", func() {
			// The token was issued with a 1m TTL; if it is still valid and
			// unchanged after 70s, the LifetimeWatcher renewed it.
			time.Sleep(70 * time.Second)

			token, err := lastPushedToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).To(Equal(firstToken), "token should be renewed in place, not rotated")

			_, err = tokenLookup(token)
			Expect(err).NotTo(HaveOccurred(), "renewed token should still be valid")
		})

		It("rotates at max TTL, pushes the replacement, and revokes the old token", func() {
			var newToken string
			Eventually(func(g Gomega) {
				token, err := lastPushedToken()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(token).NotTo(Equal(firstToken), "a rotated token should have been pushed")
				newToken = token
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			_, err := tokenLookup(newToken)
			Expect(err).NotTo(HaveOccurred(), "rotated token should be valid")

			Eventually(func(g Gomega) {
				g.Expect(tokenRevoked(firstToken)).To(BeTrue(), "old token should be revoked after the grace period")
			}, 60*time.Second, 5*time.Second).Should(Succeed())
		})
	})

	Context("with kubernetes auth, self-review mode (long-lived SA token)", func() {
		It("logs in via a reviewer-less mount where Vault's pod SA performs the TokenReview", func() {
			if external {
				Skip("self-review requires an in-cluster Vault")
			}

			By("minting a long-lived legacy token Secret for the minter's ServiceAccount")
			Expect(utils.KubectlApplyStdin(fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: mint4v-legacy-token
  namespace: %s
  annotations:
    kubernetes.io/service-account.name: %s
type: kubernetes.io/service-account-token
`, namespace, saName))).To(Succeed())
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "secret", "mint4v-legacy-token",
					"-n", namespace, "-o", "jsonpath={.data.token}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty())
			}, 30*time.Second, 2*time.Second).Should(Succeed())

			before, _ := lastPushedToken()
			helmDeploy("kubernetes", "kubernetes-selfreview", "tokenSecretName: mint4v-legacy-token")

			var token string
			Eventually(func(g Gomega) {
				var err error
				token, err = lastPushedToken()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(token).NotTo(Equal(before))
			}, 90*time.Second, 2*time.Second).Should(Succeed())

			path, err := tokenLookup(token)
			Expect(err).NotTo(HaveOccurred())
			Expect(path).To(ContainSubstring("auth/kubernetes-selfreview/login"))
		})
	})

	Context("with kubernetes auth, client-JWT review (ephemeral token + auth-delegator)", func() {
		It("logs in via an external-style mount where the client JWT reviews itself", func() {
			before, _ := lastPushedToken()
			// tokenAudience "" projects a default-audience token, which is a
			// valid apiserver credential — required for it to perform its
			// own TokenReview.
			helmDeploy("kubernetes", mountPath("kubernetes-external"), `tokenAudience: ""`)

			var token string
			Eventually(func(g Gomega) {
				var err error
				token, err = lastPushedToken()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(token).NotTo(Equal(before))
			}, 90*time.Second, 2*time.Second).Should(Succeed())

			path, err := tokenLookup(token)
			Expect(err).NotTo(HaveOccurred())
			Expect(path).To(ContainSubstring("auth/" + mountPath("kubernetes-external") + "/login"))
		})
	})

	Context("when the token is revoked out from under it", func() {
		It("recovers by minting and pushing a replacement", func() {
			current, err := lastPushedToken()
			Expect(err).NotTo(HaveOccurred())

			By("revoking the live token as an administrator")
			_, err = activeVaultClient.Logical().Write("auth/token/revoke", map[string]any{"token": current})
			Expect(err).NotTo(HaveOccurred())

			// The LifetimeWatcher's next renewal fails, which triggers a
			// fresh login and push.
			Eventually(func(g Gomega) {
				token, err := lastPushedToken()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(token).NotTo(Equal(current), "a replacement token should have been pushed")
				_, err = tokenLookup(token)
				g.Expect(err).NotTo(HaveOccurred(), "replacement token should be valid")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})
	})

	Context("with jwt auth", func() {
		It("logs in via the jwt mount and pushes a valid token", func() {
			before, _ := lastPushedToken()
			helmDeploy("jwt", mountPath("jwt"), "")

			var token string
			Eventually(func(g Gomega) {
				var err error
				token, err = lastPushedToken()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(token).NotTo(Equal(before))
			}, 90*time.Second, 2*time.Second).Should(Succeed())

			path, err := tokenLookup(token)
			Expect(err).NotTo(HaveOccurred())
			Expect(path).To(ContainSubstring("auth/" + mountPath("jwt") + "/login"))
		})
	})

	Context("with a Vault Enterprise namespace", func() {
		It("mints, renews, and revokes entirely within the namespace", func() {
			if external {
				Skip("external mode already runs inside E2E_VAULT_NAMESPACE")
			}
			if vaultLicense == "" {
				Skip("VAULT_LICENSE not set; dev Vault is OSS")
			}
			const vaultNS = "mint4v-e2e-ns"

			By("creating the Vault namespace and a kubernetes auth mount inside it")
			_, err := vaultClient.Logical().Write("sys/namespaces/"+vaultNS, map[string]any{})
			Expect(err).NotTo(HaveOccurred())

			nsClient, err := vaultClient.CloneWithHeaders()
			Expect(err).NotTo(HaveOccurred())
			nsClient.SetToken(vaultClient.Token())
			nsClient.SetNamespace(vaultNS)
			err = nsClient.Sys().EnableAuthWithOptions("kubernetes", &api.EnableAuthOptions{Type: "kubernetes"})
			if err != nil && !strings.Contains(err.Error(), "already in use") {
				Expect(err).NotTo(HaveOccurred())
			}
			_, err = nsClient.Logical().Write("auth/kubernetes/config", map[string]any{
				"kubernetes_host":    kubernetesHost(),
				"kubernetes_ca_cert": caPEM,
				"token_reviewer_jwt": reviewerJWT,
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = nsClient.Logical().Write("auth/kubernetes/role/mint4v", map[string]any{
				"bound_service_account_names":      saName,
				"bound_service_account_namespaces": namespace,
				"audience":                         "vault",
				"token_policies":                   "default",
				"token_ttl":                        tokenTTL,
				"token_max_ttl":                    tokenMaxTTL,
			})
			Expect(err).NotTo(HaveOccurred())

			// Later lookups (including the shutdown spec) must address the
			// namespace the deployment now mints in.
			deployVaultNamespace = vaultNS
			activeVaultClient = nsClient

			before, _ := lastPushedToken()
			helmDeploy("kubernetes", "kubernetes", "")

			var first string
			Eventually(func(g Gomega) {
				var err error
				first, err = lastPushedToken()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(first).NotTo(Equal(before))
			}, 90*time.Second, 2*time.Second).Should(Succeed())

			path, err := tokenLookup(first)
			Expect(err).NotTo(HaveOccurred())
			Expect(path).To(ContainSubstring("auth/kubernetes/login"))

			// Rotation and revocation must both ride the X-Vault-Namespace
			// header: the replacement is minted in the namespace and the old
			// token is revoked there.
			var rotated string
			Eventually(func(g Gomega) {
				token, err := lastPushedToken()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(token).NotTo(Equal(first), "a rotated token should have been pushed")
				rotated = token
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			_, err = tokenLookup(rotated)
			Expect(err).NotTo(HaveOccurred(), "rotated token should be valid in the namespace")

			Eventually(func(g Gomega) {
				g.Expect(tokenRevoked(first)).To(BeTrue(), "old token should be revoked within the namespace")
			}, 60*time.Second, 5*time.Second).Should(Succeed())
		})
	})

	Context("on shutdown", func() {
		It("revokes the live token when the deployment is scaled down", func() {
			if os.Getenv("E2E_KEEP") == "true" {
				Skip("E2E_KEEP=true: leaving the deployment running")
			}

			token, err := lastPushedToken()
			Expect(err).NotTo(HaveOccurred())
			_, err = tokenLookup(token)
			Expect(err).NotTo(HaveOccurred(), "token should be valid before shutdown")

			_, err = utils.Run(exec.Command("kubectl", "scale", "deployment", releaseName,
				"-n", namespace, "--replicas=0"))
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.Run(exec.Command("kubectl", "wait", "--for=delete", "pod",
				"-l", "app.kubernetes.io/instance="+releaseName, "-n", namespace, "--timeout=90s"))
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				g.Expect(tokenRevoked(token)).To(BeTrue(), "token should be revoked on SIGTERM")
			}, 30*time.Second, 2*time.Second).Should(Succeed())
		})
	})
})
