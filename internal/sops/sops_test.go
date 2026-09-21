package sops_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/sops"
)

// The fixture age key pair test/e2e/fixtures/age-keys.txt protects nothing;
// its public key is also what test/e2e/fixtures/platform-repo/platform.yaml
// carries as agePublicKey.
const testRecipient = "age1kpq9t46wreydm6dp2e9a6txzm88ymqj9ph38jvjlsjgff3k5vfqqqhee6v"

func requireSops(t *testing.T) {
	t.Helper()
	if !sops.Available() {
		t.Skip("sops is not on PATH; skipping (see docs/implementation-notes/16-cli-secret-set.md)")
	}
}

func TestBinaryEncryptProducesAKsopsDocument(t *testing.T) {
	requireSops(t)
	plaintext := []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: shop-api-key\ntype: Opaque\nstringData:\n  API_KEY: hunter2\n")

	out, err := sops.Binary{}.Encrypt(context.Background(), plaintext, testRecipient, "applications/shop/prod/sops/api-key.enc.yaml")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got := string(out)

	if strings.Contains(got, "hunter2") {
		t.Fatalf("the plaintext value leaked into the encrypted document:\n%s", got)
	}
	for _, want := range []string{
		"name: shop-api-key",
		"stringData:\n    API_KEY: ENC[",
		"sops:",
		"age:",
		"recipient: " + testRecipient,
		"encrypted_regex: ^(data|stringData)$",
		"mac: ENC[",
		"version:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestBinaryEncryptErrorsClearlyWhenSopsIsMissing(t *testing.T) {
	// An empty PATH makes exec.LookPath("sops") fail regardless of the
	// machine running the test, so this does not depend on sops being
	// absent from the developer's own PATH.
	t.Setenv("PATH", "")

	_, err := sops.Binary{}.Encrypt(context.Background(), []byte("{}"), testRecipient, "x.enc.yaml")
	if err == nil {
		t.Fatal("want an error when sops is not installed")
	}
	if !strings.Contains(err.Error(), "sops") {
		t.Errorf("error = %q, want it to mention sops", err)
	}
}

func TestAvailableMatchesLookPath(t *testing.T) {
	_, lookErr := exec.LookPath("sops")
	if got, want := sops.Available(), lookErr == nil; got != want {
		t.Errorf("Available() = %v, want %v", got, want)
	}
}
