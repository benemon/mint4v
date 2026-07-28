//nolint:staticcheck // dot-imports are the ginkgo convention
package e2e

import (
	"fmt"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"mint4v/test/utils"
)

const (
	minterImage = "mint4v:e2e"
	mockImage   = "mockcpd:e2e"
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintln(GinkgoWriter, "Starting mint4v e2e suite")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	By("building the minter image")
	_, err := utils.Run(exec.Command("make", "docker-build", "IMG="+minterImage))
	Expect(err).NotTo(HaveOccurred())

	By("building the mock CP4D image")
	_, err = utils.Run(exec.Command("make", "docker-build-mock", "MOCK_IMG="+mockImage))
	Expect(err).NotTo(HaveOccurred())

	By("loading images into the KIND cluster")
	Expect(utils.LoadImageToKindCluster(minterImage)).To(Succeed())
	Expect(utils.LoadImageToKindCluster(mockImage)).To(Succeed())
})
