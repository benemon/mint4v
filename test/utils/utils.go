// Package utils provides shell-out helpers for the e2e suite, following the
// conventions of sibling projects (commands run from the project root against
// the ambient kubeconfig that `kind create cluster` sets).
package utils

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

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

// PortForward starts `kubectl port-forward` to the given target (e.g.
// "svc/vault") and blocks until the tunnel is proven usable. kubectl exits if
// a proxied connection fails (e.g. the backend has not bound its port yet),
// so the forwarder is restarted until it survives a probe connection. The
// returned function stops forwarding.
func PortForward(namespace, target string, localPort, remotePort int) (func(), error) {
	start := func() (*exec.Cmd, chan struct{}, error) {
		cmd := exec.Command("kubectl", "port-forward", "-n", namespace, target,
			fmt.Sprintf("%d:%d", localPort, remotePort))
		cmd.Dir = ProjectRoot()
		cmd.Stdout = GinkgoWriter
		cmd.Stderr = GinkgoWriter
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		return cmd, done, nil
	}

	cmd, done, err := start()
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			time.Sleep(time.Second)
			if cmd, done, err = start(); err != nil {
				return nil, err
			}
			continue
		default:
		}
		conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), 200*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			// The local listener opens before the tunnel is exercised; only
			// return once the forwarder has survived the probe connection.
			time.Sleep(500 * time.Millisecond)
			select {
			case <-done:
				continue
			default:
				return func() { _ = cmd.Process.Kill() }, nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	return nil, fmt.Errorf("port-forward to %s in %s did not become ready", target, namespace)
}
