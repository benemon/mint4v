//nolint:staticcheck // dot-imports are the ginkgo convention
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"mint4v/test/utils"
)

// The suite runs in one of two modes:
//
//   - KIND (default): provisions a dev-mode Vault in-cluster, builds the
//     images locally, and loads them into the KIND cluster named by
//     KIND_CLUSTER.
//   - External (E2E_EXTERNAL=true): targets the current kubeconfig context
//     and an existing Vault. Requires E2E_VAULT_ADDR, E2E_VAULT_TOKEN, and
//     E2E_KUBERNETES_HOST (the API URL Vault should use for TokenReview);
//     honours E2E_VAULT_NAMESPACE and VAULT_CACERT. Images must already be
//     pullable at E2E_IMG / E2E_MOCK_IMG (see `make ocp-build`), and
//     E2E_KEEP=true leaves the deployment running after the suite.
var (
	external = os.Getenv("E2E_EXTERNAL") == "true"

	mint4vImage = envOr("E2E_IMG", "mint4v:e2e")
	mockImage   = envOr("E2E_MOCK_IMG", "mockcpd:e2e")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintln(GinkgoWriter, "Starting mint4v e2e suite")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	if external {
		for _, required := range []string{"E2E_VAULT_ADDR", "E2E_VAULT_TOKEN", "E2E_KUBERNETES_HOST"} {
			Expect(os.Getenv(required)).NotTo(BeEmpty(), required+" must be set in external mode")
		}
		return
	}

	By("building the mint4v image")
	_, err := utils.Run(exec.Command("make", "docker-build", "IMG="+mint4vImage))
	Expect(err).NotTo(HaveOccurred())

	By("building the mock CP4D image")
	_, err = utils.Run(exec.Command("make", "docker-build-mock", "MOCK_IMG="+mockImage))
	Expect(err).NotTo(HaveOccurred())

	By("loading images into the KIND cluster")
	Expect(utils.LoadImageToKindCluster(mint4vImage)).To(Succeed())
	Expect(utils.LoadImageToKindCluster(mockImage)).To(Succeed())
})
