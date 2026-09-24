// Package infra_test runs the shell tests of infra/tofu-env.sh
// (test/tofu-env/run.sh) from go test, so the Go CI job's `go test ./...`
// runs them without a workflow change of their own. The tests themselves
// stay in bash: what they check is how bash and zsh source the script.
package infra_test

import (
	"os/exec"
	"testing"
)

func TestTofuEnvScript(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	out, err := exec.Command(bash, "../test/tofu-env/run.sh").CombinedOutput()
	t.Logf("test/tofu-env/run.sh:\n%s", out)
	if err != nil {
		t.Fatalf("test/tofu-env/run.sh failed: %v", err)
	}
}
