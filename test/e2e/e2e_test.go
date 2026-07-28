//nolint:staticcheck // dot-imports are the ginkgo convention
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"mint4v/test/utils"
)

const (
	namespace   = "mint4v-e2e"
	releaseName = "mint4v"
	// The ServiceAccount the chart creates (fullname == release name).
	saName = releaseName

	mockUsername = "cpdadmin"
	mockAPIKey   = "e2e-api-key"

	// Deliberately short so renewal and rotation happen inside the test run:
	// tokens live 1m and hit max TTL (forcing rotation) at 2m.
	tokenTTL    = "1m"
	tokenMaxTTL = "2m"
)

// vaultExec runs a shell script inside the Vault pod with root credentials.
func vaultExec(script string) (string, error) {
	cmd := exec.Command("kubectl", "exec", "vault", "-n", namespace, "--", "sh", "-c",
		"export VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=e2e-root; "+script)
	return utils.Run(cmd)
}

// clusterGet fetches a URL from inside the cluster, using the Vault pod's
// wget as an in-cluster HTTP client.
func clusterGet(url string) (string, error) {
	cmd := exec.Command("kubectl", "exec", "vault", "-n", namespace, "--", "wget", "-qO-", "-T", "5", url)
	return utils.Run(cmd)
}

// lastPushedToken returns the Vault token most recently pushed to mockcpd.
func lastPushedToken() (string, error) {
	out, err := clusterGet("http://mockcpd:8080/last")
	if err != nil {
		return "", err
	}
	var payload struct {
		Details struct {
			AccessToken string `json:"access_token"`
		} `json:"details"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return "", fmt.Errorf("mock payload is not the expected JSON: %q", out)
	}
	if payload.Details.AccessToken == "" {
		return "", fmt.Errorf("mock payload has empty details.access_token: %q", out)
	}
	return payload.Details.AccessToken, nil
}

// tokenLookup runs `vault token lookup` for the given token and returns its
// output; the error is non-nil once the token is revoked or expired.
func tokenLookup(token string) (string, error) {
	return vaultExec("vault token lookup " + token)
}

// minterValues renders the chart values for a given auth method and mount.
// chartExtras is appended verbatim as extra top-level values (e.g. to switch
// the token source).
func minterValues(method, mount, chartExtras string) string {
	return fmt.Sprintf(`
image:
  repository: mint4v
  tag: e2e
  pullPolicy: Never

credentials: '{"username":"%[5]s","api_key":"%[6]s"}'
%[3]s

config: |
  log_level = "debug"

  vault {
    address      = "http://vault.%[1]s.svc:8200"
    revoke_grace = "5s"

    auth {
      method     = "%[2]s"
      mount_path = "%[4]s"
      role       = "mint4v"
      token_file = "/var/run/secrets/minter/token"
    }
  }

  push {
    url    = "http://mockcpd.%[1]s.svc:8080/zen-data/v2/vaults/1000330999:e2e-vault?validate_and_save=true"
    method = "PATCH"

    headers = {
      "Content-Type" = "application/json"
    }

    extra = {
      vault_address = "http://vault.%[1]s.svc:8200"
    }

    body_template = <<-EOT
      {"details":{"vault_address":"{{ .Extra.vault_address }}","access_token":"{{ .VaultToken }}"}}
    EOT

    login {
      url         = "http://mockcpd.%[1]s.svc:8080/icp4d-api/v1/authorize"
      token_field = "token"

      body_template = <<-EOT
        {"username":"{{ .Credentials.username }}","api_key":"{{ .Credentials.api_key }}"}
      EOT

      credentials_file = "/etc/minter/credentials/credentials.json"
    }
  }
`, namespace, method, chartExtras, mount, mockUsername, mockAPIKey)
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
    image: hashicorp/vault:1.18
    args: ["server", "-dev", "-dev-root-token-id=e2e-root", "-dev-listen-address=0.0.0.0:8200"]
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
`, namespace))).To(Succeed())
		_, err := utils.Run(exec.Command("kubectl", "wait", "--for=condition=Ready", "pod/vault",
			"-n", namespace, "--timeout=120s"))
		Expect(err).NotTo(HaveOccurred())

		By("deploying the mock CP4D API")
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
        imagePullPolicy: Never
        env:
        - name: MOCK_USERNAME
          value: %[3]s
        - name: MOCK_APIKEY
          value: %[4]s
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
`, namespace, mockImage, mockUsername, mockAPIKey))).To(Succeed())
		_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/mockcpd",
			"-n", namespace, "--timeout=120s"))
		Expect(err).NotTo(HaveOccurred())

		By("granting Vault's ServiceAccount TokenReview access")
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
  name: mint4v-e2e-oidc-discovery
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:service-account-issuer-discovery
subjects:
- kind: Group
  name: system:unauthenticated
  apiGroup: rbac.authorization.k8s.io
`, namespace))).To(Succeed())

		By("configuring Vault's kubernetes auth method")
		reviewerJWT, err := utils.Run(exec.Command("kubectl", "create", "token", "vault-reviewer",
			"-n", namespace, "--duration=2h"))
		Expect(err).NotTo(HaveOccurred())
		reviewerJWT = strings.TrimSpace(reviewerJWT)

		_, err = vaultExec("vault auth enable kubernetes || true")
		Expect(err).NotTo(HaveOccurred())
		_, err = vaultExec(fmt.Sprintf(
			"vault write auth/kubernetes/config kubernetes_host=https://kubernetes.default.svc "+
				"kubernetes_ca_cert=@/var/run/secrets/kubernetes.io/serviceaccount/ca.crt token_reviewer_jwt=%q", reviewerJWT))
		Expect(err).NotTo(HaveOccurred())
		_, err = vaultExec(fmt.Sprintf(
			"vault write auth/kubernetes/role/mint4v bound_service_account_names=%s bound_service_account_namespaces=%s "+
				"audience=vault token_policies=default token_ttl=%s token_max_ttl=%s", saName, namespace, tokenTTL, tokenMaxTTL))
		Expect(err).NotTo(HaveOccurred())

		// Reviewer-less mount: with no token_reviewer_jwt, the kubernetes
		// auth plugin performs the TokenReview as VAULT'S OWN pod
		// ServiceAccount, which therefore needs auth-delegator. The client
		// logs in with a long-lived legacy Secret token (no audience).
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
		_, err = vaultExec("vault auth enable -path=kubernetes-selfreview kubernetes || true")
		Expect(err).NotTo(HaveOccurred())
		_, err = vaultExec("vault write auth/kubernetes-selfreview/config kubernetes_host=https://kubernetes.default.svc " +
			"kubernetes_ca_cert=@/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
		Expect(err).NotTo(HaveOccurred())
		_, err = vaultExec(fmt.Sprintf(
			"vault write auth/kubernetes-selfreview/role/mint4v bound_service_account_names=%s "+
				"bound_service_account_namespaces=%s token_policies=default token_ttl=%s token_max_ttl=%s",
			saName, namespace, tokenTTL, tokenMaxTTL))
		Expect(err).NotTo(HaveOccurred())

		// External-Vault semantics: disable_local_ca_jwt=true stops the
		// plugin from adopting its pod credentials, so the CLIENT's login
		// JWT performs its own TokenReview. The minter's SA needs
		// auth-delegator and its JWT must be a valid apiserver credential
		// (a projected token with the default audience).
		By("configuring an external-style kubernetes auth mount (client JWT self-review)")
		Expect(utils.KubectlApplyStdin(fmt.Sprintf(`
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
  name: %s
  namespace: %s
`, saName, namespace))).To(Succeed())
		_, err = vaultExec("vault auth enable -path=kubernetes-external kubernetes || true")
		Expect(err).NotTo(HaveOccurred())
		_, err = vaultExec("vault write auth/kubernetes-external/config kubernetes_host=https://kubernetes.default.svc " +
			"kubernetes_ca_cert=@/var/run/secrets/kubernetes.io/serviceaccount/ca.crt disable_local_ca_jwt=true")
		Expect(err).NotTo(HaveOccurred())
		_, err = vaultExec(fmt.Sprintf(
			"vault write auth/kubernetes-external/role/mint4v bound_service_account_names=%s "+
				"bound_service_account_namespaces=%s token_policies=default token_ttl=%s token_max_ttl=%s",
			saName, namespace, tokenTTL, tokenMaxTTL))
		Expect(err).NotTo(HaveOccurred())

		By("configuring Vault's jwt auth method")
		// Vault requires oidc_discovery_url to exactly match the cluster's
		// service-account issuer (in KIND that is
		// https://kubernetes.default.svc.cluster.local), so read it from the
		// discovery document rather than hardcoding it.
		discovery, err := utils.Run(exec.Command("kubectl", "get", "--raw", "/.well-known/openid-configuration"))
		Expect(err).NotTo(HaveOccurred())
		var oidc struct {
			Issuer string `json:"issuer"`
		}
		Expect(json.Unmarshal([]byte(discovery), &oidc)).To(Succeed())
		Expect(oidc.Issuer).NotTo(BeEmpty())

		_, err = vaultExec("vault auth enable jwt || true")
		Expect(err).NotTo(HaveOccurred())
		_, err = vaultExec(fmt.Sprintf("vault write auth/jwt/config oidc_discovery_url=%s "+
			"oidc_discovery_ca_pem=@/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", oidc.Issuer))
		Expect(err).NotTo(HaveOccurred())
		_, err = vaultExec(fmt.Sprintf(
			"vault write auth/jwt/role/mint4v role_type=jwt user_claim=sub bound_audiences=vault "+
				"bound_subject=system:serviceaccount:%s:%s token_policies=default token_ttl=%s token_max_ttl=%s",
			namespace, saName, tokenTTL, tokenMaxTTL))
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		_, _ = utils.Run(exec.Command("helm", "uninstall", releaseName, "-n", namespace, "--ignore-not-found"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding",
			"mint4v-e2e-vault-reviewer", "mint4v-e2e-oidc-discovery",
			"mint4v-e2e-selfreview", "mint4v-e2e-clientreview", "--ignore-not-found"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found", "--timeout=120s"))
	})

	Context("with kubernetes auth", func() {
		var firstToken string

		It("logs in, performs the CP4D-style pre-login, and pushes a valid token", func() {
			helmDeploy("kubernetes", "kubernetes", "")

			var token string
			Eventually(func(g Gomega) {
				var err error
				token, err = lastPushedToken()
				g.Expect(err).NotTo(HaveOccurred())
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			out, err := tokenLookup(token)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("auth/kubernetes/login"))
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
				_, err := tokenLookup(firstToken)
				g.Expect(err).To(HaveOccurred(), "old token should be revoked after the grace period")
			}, 60*time.Second, 5*time.Second).Should(Succeed())
		})
	})

	Context("with kubernetes auth, self-review mode (long-lived SA token)", func() {
		It("logs in via a reviewer-less mount where Vault's pod SA performs the TokenReview", func() {
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

			out, err := tokenLookup(token)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("auth/kubernetes-selfreview/login"))
		})
	})

	Context("with kubernetes auth, client-JWT review (ephemeral token + auth-delegator)", func() {
		It("logs in via an external-style mount where the client JWT reviews itself", func() {
			before, _ := lastPushedToken()
			// tokenAudience "" projects a default-audience token, which is a
			// valid apiserver credential — required for it to perform its
			// own TokenReview.
			helmDeploy("kubernetes", "kubernetes-external", `tokenAudience: ""`)

			var token string
			Eventually(func(g Gomega) {
				var err error
				token, err = lastPushedToken()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(token).NotTo(Equal(before))
			}, 90*time.Second, 2*time.Second).Should(Succeed())

			out, err := tokenLookup(token)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("auth/kubernetes-external/login"))
		})
	})

	Context("with jwt auth", func() {
		It("logs in via the jwt mount and pushes a valid token", func() {
			before, _ := lastPushedToken()
			helmDeploy("jwt", "jwt", "")

			var token string
			Eventually(func(g Gomega) {
				var err error
				token, err = lastPushedToken()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(token).NotTo(Equal(before))
			}, 90*time.Second, 2*time.Second).Should(Succeed())

			out, err := tokenLookup(token)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("auth/jwt/login"))
		})
	})

	Context("on shutdown", func() {
		It("revokes the live token when the deployment is scaled down", func() {
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
				_, err := tokenLookup(token)
				g.Expect(err).To(HaveOccurred(), "token should be revoked on SIGTERM")
			}, 30*time.Second, 2*time.Second).Should(Succeed())
		})
	})
})
