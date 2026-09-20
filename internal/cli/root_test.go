package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/version"
)

// run invokes the CLI in-process, the way main does, and returns what it
// wrote and the exit code it would have exited with.
func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cli.Run(args, strings.NewReader(""), &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestVersionPrintsInjectedVersion(t *testing.T) {
	previous := version.Version
	version.Version = "1.2.3"
	t.Cleanup(func() { version.Version = previous })

	stdout, stderr, code := run(t, "version")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "1.2.3\n" {
		t.Fatalf("stdout = %q, want %q", stdout, "1.2.3\n")
	}
}

func TestVersionDefaultsToDevWhenNothingInjected(t *testing.T) {
	stdout, _, code := run(t, "version")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stdout != "dev\n" {
		t.Fatalf("stdout = %q, want %q", stdout, "dev\n")
	}
}

func TestUnknownCommandFails(t *testing.T) {
	stdout, stderr, code := run(t, "frobnicate")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing", stdout)
	}
	if !strings.Contains(stderr, "frobnicate") {
		t.Fatalf("stderr = %q, want it to name the unknown command", stderr)
	}
}
