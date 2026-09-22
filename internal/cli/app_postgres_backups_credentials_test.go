package cli_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/cli"
)

// testBackupsCredentialsPath is the real bootstrap/backups-credentials.enc.yaml
// the kind end-to-end fixture carries
// (test/e2e/fixtures/platform-repo/bootstrap/backups-credentials.enc.yaml),
// the same file a real bootstrap wizard run writes once for the Platform's
// age key. Tests here reference it directly, rather than duplicating its
// bytes, so the CLI's byte-for-byte copy is proven against the exact
// content the e2e fixture (and a real Platform) would carry.
const testBackupsCredentialsPath = "../../test/e2e/fixtures/platform-repo/bootstrap/backups-credentials.enc.yaml"

// readBackupsCredentialsFixture reads testBackupsCredentialsPath.
func readBackupsCredentialsFixture(t *testing.T) []byte {
	t.Helper()
	abs, err := filepath.Abs(testBackupsCredentialsPath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// assertBackupsCredentialsCopied asserts that clone's
// applications/<app>/<environment>/sops/backups-credentials.enc.yaml is a
// byte-for-byte copy of the fixture file, is listed in that directory's
// ksops.yaml, and -- when sops is on PATH -- decrypts with the fixture
// private key to the expected Secret: no namespace (valid in any
// Environment), the needs-hash annotation, and the two Object Storage
// keys. Decrypting a file that lives under a different path than the one
// it was originally encrypted at is itself part of what is being proven:
// SOPS's MAC covers the values, not the document's location
// (docs/implementation-notes/42-backups-credentials.md).
func assertBackupsCredentialsCopied(t *testing.T, clone, app, environment string) {
	t.Helper()
	want := readBackupsCredentialsFixture(t)
	sopsDir := filepath.Join(clone, "applications", app, environment, "sops")
	encPath := filepath.Join(sopsDir, "backups-credentials.enc.yaml")

	got, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatalf("reading %s: %v", encPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is not a byte-for-byte copy of %s", encPath, testBackupsCredentialsPath)
	}

	ksops := readYAML(t, filepath.Join(sopsDir, "ksops.yaml"))
	files, _ := ksops["files"].([]any)
	found := false
	for _, f := range files {
		if f == "backups-credentials.enc.yaml" {
			found = true
		}
	}
	if !found {
		t.Errorf("ksops.yaml files = %v, want it to list backups-credentials.enc.yaml", files)
	}

	if sopsAvailable() {
		doc := decryptSOPSFile(t, encPath)
		metadata, _ := doc["metadata"].(map[string]any)
		if _, hasNamespace := metadata["namespace"]; hasNamespace {
			t.Errorf("decrypted Secret carries a namespace (%v), want none: it must be valid in any Environment", metadata["namespace"])
		}
		if got := metadata["name"]; got != "backups-credentials" {
			t.Errorf("decrypted Secret name = %v, want backups-credentials", got)
		}
		annotations, _ := metadata["annotations"].(map[string]any)
		if got := annotations["kustomize.config.k8s.io/needs-hash"]; got != "false" {
			t.Errorf(`decrypted Secret annotation needs-hash = %v, want "false"`, got)
		}
		stringData, _ := doc["stringData"].(map[string]any)
		if _, ok := stringData["ACCESS_KEY_ID"]; !ok {
			t.Errorf("decrypted Secret has no ACCESS_KEY_ID: %v", stringData)
		}
		if _, ok := stringData["ACCESS_SECRET_KEY"]; !ok {
			t.Errorf("decrypted Secret has no ACCESS_SECRET_KEY: %v", stringData)
		}
	}

	// The chart references this Secret through
	// platform.backupsCredentialsSecret, not envFrom, so it must never be
	// added to values.yaml's secrets: list (docs/implementation-notes/42-backups-credentials.md).
	values, err := os.ReadFile(filepath.Join(clone, "applications", app, environment, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(values), "backups-credentials") {
		t.Errorf("%s/values.yaml mentions backups-credentials, want it absent from secrets:", environment)
	}

	assertOneSopsSource(t, filepath.Join(clone, "applications", app, environment, "application.yaml"), app, environment)
}

// assertOneSopsSource asserts that applicationYAMLPath's spec.sources lists
// the Environment's sops/ directory exactly once, however many callers
// (iidp secret set, then iidp app create/add-capability --postgres) added
// to it: render.AddKustomizeSource matches by path and must stay a no-op
// once the source already exists.
func assertOneSopsSource(t *testing.T, applicationYAMLPath, app, environment string) {
	t.Helper()
	doc := readYAML(t, applicationYAMLPath)
	spec, _ := doc["spec"].(map[string]any)
	sources, _ := spec["sources"].([]any)
	wantPath := "applications/" + app + "/" + environment + "/sops"
	count := 0
	for _, s := range sources {
		src, _ := s.(map[string]any)
		if src["path"] == wantPath {
			count++
		}
	}
	if count != 1 {
		t.Errorf("application.yaml spec.sources has the sops source %d times, want exactly once (sources: %v)", count, sources)
	}
}

// sopsAvailable reports whether sops is on PATH, without failing or
// skipping the whole test: the byte-for-byte and ksops.yaml assertions in
// assertBackupsCredentialsCopied must still run either way; only the
// decrypt round trip needs the binary, exactly as
// docs/implementation-notes/16-cli-secret-set.md already established for
// iidp secret set's own tests (requireSops, which does skip the whole
// test, is used directly where the entire test is about the decrypted
// content).
func sopsAvailable() bool {
	_, err := exec.LookPath("sops")
	return err == nil
}

func TestAppCreatePostgresCopiesBackupsCredentials(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--postgres")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	assertBackupsCredentialsCopied(t, cloneMain(t, url), "shop", "prod")
}

func TestAppCreatePostgresCopiesBackupsCredentialsForStaging(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--postgres", "--staging")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	assertBackupsCredentialsCopied(t, clone, "shop", "prod")
	assertBackupsCredentialsCopied(t, clone, "shop", "staging")
}

func TestAppCreatePostgresRequiresBackupsCredentialsFile(t *testing.T) {
	url := newPlatformRepositoryWithoutBackupsCredentials(t, testCapabilitiesPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--postgres")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "bootstrap/backups-credentials.enc.yaml") {
		t.Errorf("stderr = %q, want it to name bootstrap/backups-credentials.enc.yaml", stderr)
	}
	if !strings.Contains(stderr, "wizard") {
		t.Errorf("stderr = %q, want it to point at the bootstrap wizard", stderr)
	}
	assertNoApplications(t, url)
}

func TestAppAddCapabilityPostgresCopiesBackupsCredentials(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)

	_, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--postgres")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	assertBackupsCredentialsCopied(t, cloneMain(t, url), "shop", "prod")
}

// TestAppAddCapabilityPostgresCopiesBackupsCredentialsWithExistingSecrets
// covers an Environment that already has another secret set
// (iidp secret set) before Postgres is added: the existing sops/ directory,
// its kustomization.yaml/ksops.yaml and the application.yaml's sops source
// (added by secret set) must all be preserved, and application.yaml's sops
// source must stay listed exactly once (render.AddKustomizeSource's own
// idempotency), not duplicated by the Postgres Capability adding it again.
func TestAppAddCapabilityPostgresCopiesBackupsCredentialsWithExistingSecrets(t *testing.T) {
	requireSops(t)
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML+"agePublicKey: "+testAgePublicKey+"\n")
	seedApplication(t, url)
	_, stderr, code := setSecret(t, url, cli.Dependencies{}, "", "shop", "prod", "API_KEY=hunter2")
	if code != 0 {
		t.Fatalf("seeding a secret: exit %d\nstderr: %s", code, stderr)
	}

	_, stderr, code = addCapability(t, url, "shop", cli.Dependencies{}, "--postgres")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	assertBackupsCredentialsCopied(t, clone, "shop", "prod")

	// The pre-existing secret's own file and ksops.yaml entry must survive.
	sopsDir := filepath.Join(clone, "applications", "shop", "prod", "sops")
	if _, err := os.Stat(filepath.Join(sopsDir, "api-key.enc.yaml")); err != nil {
		t.Errorf("the pre-existing api-key.enc.yaml is gone: %v", err)
	}
	ksops := readYAML(t, filepath.Join(sopsDir, "ksops.yaml"))
	files, _ := ksops["files"].([]any)
	if len(files) != 2 {
		t.Errorf("ksops.yaml files = %v, want exactly api-key.enc.yaml and backups-credentials.enc.yaml", files)
	}
}

func TestAppAddCapabilityStagingCopiesBackupsCredentialsWhenProdHasPostgres(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--postgres")

	_, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--staging")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	assertBackupsCredentialsCopied(t, cloneMain(t, url), "shop", "staging")
}

func TestAppAddCapabilityPostgresRequiresBackupsCredentialsFile(t *testing.T) {
	url := newPlatformRepositoryWithoutBackupsCredentials(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)

	_, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--postgres")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "bootstrap/backups-credentials.enc.yaml") {
		t.Errorf("stderr = %q, want it to name bootstrap/backups-credentials.enc.yaml", stderr)
	}
	if !strings.Contains(stderr, "wizard") {
		t.Errorf("stderr = %q, want it to point at the bootstrap wizard", stderr)
	}
	if got := headSubject(t, url); got != "iidp app create shop" {
		t.Errorf("Platform repository head is %q, want unchanged (nothing written by the refused add-capability)", got)
	}
}
