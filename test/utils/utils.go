// Package utils provides shell-out helpers for the e2e suite, following the
// conventions of sibling projects (commands run from the project root against
// the ambient kubeconfig that `kind create cluster` sets).
package utils

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // dot-import is the ginkgo convention
)

// Run executes the provided command from the project root and returns its
// combined output.
func Run(cmd *exec.Cmd) (string, error) {
	cmd.Dir = ProjectRoot()
	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %s\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%s failed: %w: %s", command, err, string(output))
	}
	return string(output), nil
}

// ProjectRoot returns the repository root, normalizing away the test/e2e
// working directory that `go test` sets.
func ProjectRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(wd, "/test/e2e", "")
}

// LoadImageToKindCluster loads a local docker image into the KIND cluster
// named by the KIND_CLUSTER env var (default "kind").
func LoadImageToKindCluster(name string) error {
	cluster := "kind"
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}
	_, err := Run(exec.Command("kind", "load", "docker-image", name, "--name", cluster))
	return err
}

// KubectlApplyStdin applies a YAML manifest supplied as a string.
func KubectlApplyStdin(manifest string, extraArgs ...string) error {
	args := append([]string{"apply", "-f", "-"}, extraArgs...)
	cmd := exec.Command("kubectl", args...)
	cmd.Stdin = strings.NewReader(manifest)
	_, err := Run(cmd)
	return err
}
