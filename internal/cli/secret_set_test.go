package cli_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/platform"
)

// The fixture age key pair: test/e2e/fixtures/age-keys.txt is the private
// key matching the agePublicKey below and in
// test/e2e/fixtures/platform-repo/platform.yaml. It protects nothing.
const (
	testAgePublicKey   = "age1kpq9t46wreydm6dp2e9a6txzm88ymqj9ph38jvjlsjgff3k5vfqqqhee6v"
	testAgeKeyFilePath = "../../test/e2e/fixtures/age-keys.txt"
)

const testPlatformYAMLWithAgeKey = testPlatformYAML + "agePublicKey: " + testAgePublicKey + "\n"

// requireSops skips the test when the sops binary is not on PATH, the way
// the chart tests skip when helm or kubeconform are missing: the Go CI job
// stays tool-free (see docs/implementation-notes/16-cli-secret-set.md).
func requireSops(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sops"); err != nil {
		t.Skip("sops is not on PATH; skipping (see docs/implementation-notes/16-cli-secret-set.md)")
	}
}

// setSecret runs iidp secret set in-process against the Platform repository
// at url with a fake token, the way tests drive the seam.
func setSecret(t *testing.T, url string, deps cli.Dependencies, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if deps.TokenSource == nil {
		deps.TokenSource = fakeTokenSource{token: "gho_test"}
	}
	full := append([]string{"secret", "set", "--platform-repo", url}, args...)
	var out, errOut bytes.Buffer
	code = cli.RunWith(full, strings.NewReader(stdin), &out, &errOut, deps)
	return out.String(), errOut.String(), code
}

// createShopApplication seeds the Platform repository with a prod
// Environment for "shop" through the real app create command, so secret set
// tests have an Environment directory to write into.
func createShopApplication(t *testing.T, url string) {
	t.Helper()
	_, stderr, code := createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")
	if code != 0 {
		t.Fatalf("seeding shop failed: exit %d\n%s", code, stderr)
	}
}

// decryptSOPSFile decrypts path with the fixture private key and returns
// the parsed YAML, proving the document is a valid, decryptable SOPS
// document (round trip), not merely shaped like one.
func decryptSOPSFile(t *testing.T, path string) map[string]any {
	t.Helper()
	keyFile, err := filepath.Abs(testAgeKeyFilePath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sops", "--decrypt", path)
	cmd.Env = append(os.Environ(), "SOPS_AGE_KEY_FILE="+keyFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sops --decrypt %s: %v\n%s", path, err, out)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("parsing decrypted %s: %v\n%s", path, err, out)
	}
	return doc
}

// assertPlaintextAbsent fails the test if plaintext appears anywhere in the
// committed tree at dir. git grep exits 1 when nothing matches, which is
// the success case here.
func assertPlaintextAbsent(t *testing.T, dir, plaintext string) {
	t.Helper()
	cmd := exec.Command("git", "grep", "-I", "-l", plaintext)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Errorf("plaintext %q found in the committed tree:\n%s", plaintext, out)
		return
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("git grep %q: %v\n%s", plaintext, err, out)
	}
}

func TestSecretSetEncryptsAndCommitsAKeyFromAFlag(t *testing.T) {
	requireSops(t)
	url := newPlatformRepository(t, testPlatformYAMLWithAgeKey)
	createShopApplication(t, url)

	stdout, stderr, code := setSecret(t, url, cli.Dependencies{}, "", "shop", "prod", "API_KEY=hunter2")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	clone := cloneMain(t, url)
	if got := strings.TrimSpace(gitRun(t, clone, "log", "-1", "--format=%s")); got != "iidp secret set shop prod API_KEY" {
		t.Errorf("commit subject = %q, want %q", got, "iidp secret set shop prod API_KEY")
	}

	encPath := filepath.Join(clone, "applications/shop/prod/sops/api-key.enc.yaml")
	doc := decryptSOPSFile(t, encPath)
	sd, ok := doc["stringData"].(map[string]any)
	if !ok || sd["API_KEY"] != "hunter2" {
		t.Errorf("decrypted stringData = %#v, want API_KEY: hunter2", doc["stringData"])
	}
	if got := doc["metadata"].(map[string]any)["name"]; got != "shop-api-key" {
		t.Errorf("Secret name = %v, want shop-api-key", got)
	}

	// The plaintext value must never appear anywhere in the committed
	// tree or the commit message.
	assertPlaintextAbsent(t, clone, "hunter2")
	if strings.Contains(headSubject(t, url), "hunter2") {
		t.Error("plaintext leaked into the commit message")
	}

	values := readYAML(t, filepath.Join(clone, "applications/shop/prod/values.yaml"))
	secrets, ok := lookup(t, values, "secrets").([]any)
	if !ok || len(secrets) != 1 || secrets[0] != "shop-api-key" {
		t.Errorf("values.yaml secrets = %v, want [shop-api-key]", lookup(t, values, "secrets"))
	}

	kustomization := readYAML(t, filepath.Join(clone, "applications/shop/prod/sops/kustomization.yaml"))
	if got := lookup(t, kustomization, "generators", 0); got != "ksops.yaml" {
		t.Errorf("kustomization.yaml generators = %v", got)
	}

	ksops := readYAML(t, filepath.Join(clone, "applications/shop/prod/sops/ksops.yaml"))
	files, ok := lookup(t, ksops, "files").([]any)
	if !ok || len(files) != 1 || files[0] != "api-key.enc.yaml" {
		t.Errorf("ksops.yaml files = %v, want [api-key.enc.yaml]", lookup(t, ksops, "files"))
	}

	app := readYAML(t, filepath.Join(clone, "applications/shop/prod/application.yaml"))
	sources, ok := lookup(t, app, "spec", "sources").([]any)
	if !ok || len(sources) != 3 {
		t.Fatalf("spec.sources has %d entries, want 3", len(sources))
	}
	third := sources[2].(map[string]any)
	if third["repoURL"] != platform.RepositoryURL || third["path"] != "applications/shop/prod/sops" || third["targetRevision"] != "main" {
		t.Errorf("third source = %#v", third)
	}
}

func TestSecretSetOnStagingUsesTheStagingObjectName(t *testing.T) {
	requireSops(t)
	url := newPlatformRepository(t, testPlatformYAMLWithAgeKey)
	_, stderr, code := createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service")
	if code != 0 {
		t.Fatalf("seeding shop failed: %s", stderr)
	}
	pushCommit(t, url, "Add a staging Environment by hand", map[string]string{
		"applications/shop/staging/values.yaml": readFile(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml")),
		"applications/shop/staging/application.yaml": strings.ReplaceAll(
			readFile(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/application.yaml")),
			"prod", "staging"),
	})

	_, stderr, code = setSecret(t, url, cli.Dependencies{}, "", "shop", "staging", "API_KEY=hunter2")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}

	clone := cloneMain(t, url)
	encPath := filepath.Join(clone, "applications/shop/staging/sops/api-key.enc.yaml")
	doc := decryptSOPSFile(t, encPath)
	if got := doc["metadata"].(map[string]any)["name"]; got != "shop-staging-api-key" {
		t.Errorf("Secret name = %v, want shop-staging-api-key", got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSecretSetAddingASecondKeyOnlyTouchesKsopsAndValues(t *testing.T) {
	requireSops(t)
	url := newPlatformRepository(t, testPlatformYAMLWithAgeKey)
	createShopApplication(t, url)
	if _, stderr, code := setSecret(t, url, cli.Dependencies{}, "", "shop", "prod", "API_KEY=hunter2"); code != 0 {
		t.Fatalf("first secret set failed: %s", stderr)
	}

	_, stderr, code := setSecret(t, url, cli.Dependencies{}, "", "shop", "prod", "DB_PASSWORD=swordfish")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}

	clone := cloneMain(t, url)
	added := strings.Fields(gitRun(t, clone, "show", "--name-only", "--format=", "HEAD"))
	want := []string{
		"applications/shop/prod/sops/db-password.enc.yaml",
		"applications/shop/prod/sops/ksops.yaml",
		"applications/shop/prod/values.yaml",
	}
	if strings.Join(added, " ") != strings.Join(want, " ") {
		t.Errorf("commit touched %v, want %v (kustomization.yaml and application.yaml must be unchanged)", added, want)
	}

	ksops := readYAML(t, filepath.Join(clone, "applications/shop/prod/sops/ksops.yaml"))
	files, _ := lookup(t, ksops, "files").([]any)
	wantFiles := []any{"api-key.enc.yaml", "db-password.enc.yaml"}
	if len(files) != 2 || files[0] != wantFiles[0] || files[1] != wantFiles[1] {
		t.Errorf("ksops.yaml files = %v, want %v", files, wantFiles)
	}

	values := readYAML(t, filepath.Join(clone, "applications/shop/prod/values.yaml"))
	secrets, _ := lookup(t, values, "secrets").([]any)
	if len(secrets) != 2 || secrets[0] != "shop-api-key" || secrets[1] != "shop-db-password" {
		t.Errorf("values.yaml secrets = %v, want [shop-api-key shop-db-password]", secrets)
	}
}

func TestSecretSetUpdatingAnExistingKeyOnlyTouchesItsFile(t *testing.T) {
	requireSops(t)
	url := newPlatformRepository(t, testPlatformYAMLWithAgeKey)
	createShopApplication(t, url)
	if _, stderr, code := setSecret(t, url, cli.Dependencies{}, "", "shop", "prod", "API_KEY=hunter2"); code != 0 {
		t.Fatalf("first secret set failed: %s", stderr)
	}

	_, stderr, code := setSecret(t, url, cli.Dependencies{}, "", "shop", "prod", "API_KEY=new-value")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}

	clone := cloneMain(t, url)
	added := strings.Fields(gitRun(t, clone, "show", "--name-only", "--format=", "HEAD"))
	want := []string{"applications/shop/prod/sops/api-key.enc.yaml"}
	if strings.Join(added, " ") != strings.Join(want, " ") {
		t.Errorf("commit touched %v, want %v", added, want)
	}

	values := readYAML(t, filepath.Join(clone, "applications/shop/prod/values.yaml"))
	secrets, _ := lookup(t, values, "secrets").([]any)
	if len(secrets) != 1 || secrets[0] != "shop-api-key" {
		t.Errorf("values.yaml secrets = %v, want [shop-api-key] (setting the same key twice must not duplicate it)", secrets)
	}

	doc := decryptSOPSFile(t, filepath.Join(clone, "applications/shop/prod/sops/api-key.enc.yaml"))
	sd, _ := doc["stringData"].(map[string]any)
	if sd["API_KEY"] != "new-value" {
		t.Errorf("stringData = %#v, want the updated value", sd)
	}
}

func TestSecretSetReadsAValueFromAFile(t *testing.T) {
	requireSops(t)
	url := newPlatformRepository(t, testPlatformYAMLWithAgeKey)
	createShopApplication(t, url)
	path := filepath.Join(t.TempDir(), "value.txt")
	if err := os.WriteFile(path, []byte("from-a-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := setSecret(t, url, cli.Dependencies{}, "", "shop", "prod", "--from-file", "API_KEY="+path)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}

	clone := cloneMain(t, url)
	doc := decryptSOPSFile(t, filepath.Join(clone, "applications/shop/prod/sops/api-key.enc.yaml"))
	sd, _ := doc["stringData"].(map[string]any)
	if sd["API_KEY"] != "from-a-file" {
		t.Errorf("stringData = %#v, want API_KEY: from-a-file (trailing newline trimmed)", sd)
	}
}

func TestSecretSetReadsAValueFromStdin(t *testing.T) {
	requireSops(t)
	url := newPlatformRepository(t, testPlatformYAMLWithAgeKey)
	createShopApplication(t, url)

	_, stderr, code := setSecret(t, url, cli.Dependencies{}, "from-stdin\n", "shop", "prod", "--stdin", "API_KEY")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}

	clone := cloneMain(t, url)
	doc := decryptSOPSFile(t, filepath.Join(clone, "applications/shop/prod/sops/api-key.enc.yaml"))
	sd, _ := doc["stringData"].(map[string]any)
	if sd["API_KEY"] != "from-stdin" {
		t.Errorf("stringData = %#v, want API_KEY: from-stdin", sd)
	}
}

func TestSecretSetRefusesAnInvalidKeyBeforeAnyWrite(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAMLWithAgeKey)
	createShopApplication(t, url)
	head := headSubject(t, url)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"lowercase-with-dash", []string{"shop", "prod", "api-key=x"}},
		{"starts-with-digit", []string{"shop", "prod", "1KEY=x"}},
		{"no-equals-sign", []string{"shop", "prod", "APIKEY"}},
		{"empty-key", []string{"shop", "prod", "=x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := setSecret(t, url, cli.Dependencies{}, "", tc.args...)
			if code == 0 {
				t.Fatalf("exit code = 0, want non-zero")
			}
			if stderr == "" {
				t.Error("stderr is empty, want an explanation")
			}
		})
	}
	if got := headSubject(t, url); got != head {
		t.Errorf("Platform repository head changed to %q, want no write on refusal", got)
	}
}

func TestSecretSetRefusesAnUnknownEnvironment(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAMLWithAgeKey)
	createShopApplication(t, url)

	_, stderr, code := setSecret(t, url, cli.Dependencies{}, "", "shop", "canary", "API_KEY=x")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "prod") || !strings.Contains(stderr, "staging") {
		t.Errorf("stderr = %q, want it to name prod and staging", stderr)
	}
}

func TestSecretSetRefusesAMissingApplication(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAMLWithAgeKey)

	_, stderr, code := setSecret(t, url, cli.Dependencies{}, "", "ghost", "prod", "API_KEY=x")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "ghost") {
		t.Errorf("stderr = %q, want it to name the missing Application", stderr)
	}
}

func TestSecretSetRefusesAMissingEnvironmentDirectory(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAMLWithAgeKey)
	createShopApplication(t, url)

	_, stderr, code := setSecret(t, url, cli.Dependencies{}, "", "shop", "staging", "API_KEY=x")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "staging") {
		t.Errorf("stderr = %q, want it to name the missing staging Environment", stderr)
	}
}

func TestSecretSetRefusesWhenAgePublicKeyIsMissing(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML) // no agePublicKey
	createShopApplication(t, url)

	_, stderr, code := setSecret(t, url, cli.Dependencies{}, "", "shop", "prod", "API_KEY=x")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "agePublicKey") || !strings.Contains(stderr, "platform.yaml") {
		t.Errorf("stderr = %q, want it to name agePublicKey and platform.yaml", stderr)
	}
}

func TestSecretSetRequiresAtLeastOneKey(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAMLWithAgeKey)
	createShopApplication(t, url)

	_, stderr, code := setSecret(t, url, cli.Dependencies{}, "", "shop", "prod")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if stderr == "" {
		t.Error("stderr is empty, want an explanation")
	}
}

func TestSecretSetRefusesADuplicateKeyInOneInvocation(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAMLWithAgeKey)
	createShopApplication(t, url)

	_, stderr, code := setSecret(t, url, cli.Dependencies{}, "", "shop", "prod", "API_KEY=one", "API_KEY=two")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "API_KEY") {
		t.Errorf("stderr = %q, want it to name the duplicated KEY", stderr)
	}
}

func TestSecretSetRetriesOnceWhenMainMoved(t *testing.T) {
	requireSops(t)
	url := newPlatformRepository(t, testPlatformYAMLWithAgeKey)
	createShopApplication(t, url)
	pushes := 0
	deps := cli.Dependencies{BeforePush: func() error {
		pushes++
		if pushes == 1 {
			pushCommit(t, url, "Hand-edit README", map[string]string{"README.md": "edited by hand\n"})
		}
		return nil
	}}

	stdout, stderr, code := setSecret(t, url, deps, "", "shop", "prod", "API_KEY=hunter2")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if pushes != 2 {
		t.Errorf("push attempts = %d, want 2", pushes)
	}
	clone := cloneMain(t, url)
	if _, err := os.Stat(filepath.Join(clone, "applications/shop/prod/sops/api-key.enc.yaml")); err != nil {
		t.Errorf("the encrypted secret is missing after retry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clone, "README.md")); err != nil {
		t.Errorf("the hand edit was lost: %v", err)
	}
}

// TestSecretSetProducesAKustomizationKsopsCanBuild renders the written
// sops/ directory with the real kustomize and ksops binaries, the way
// ArgoCD's repo server does. It only runs when both are on PATH: the Go CI
// job stays tool-free, the same convention the chart tests use for helm and
// kubeconform.
func TestSecretSetProducesAKustomizationKsopsCanBuild(t *testing.T) {
	requireSops(t)
	if _, err := exec.LookPath("kustomize"); err != nil {
		t.Skip("kustomize is not on PATH; skipping")
	}
	if _, err := exec.LookPath("ksops"); err != nil {
		t.Skip("ksops is not on PATH; skipping")
	}
	url := newPlatformRepository(t, testPlatformYAMLWithAgeKey)
	createShopApplication(t, url)
	if _, stderr, code := setSecret(t, url, cli.Dependencies{}, "", "shop", "prod", "API_KEY=hunter2"); code != 0 {
		t.Fatalf("secret set failed: %s", stderr)
	}
	clone := cloneMain(t, url)
	sopsDir := filepath.Join(clone, "applications/shop/prod/sops")

	keyFile, err := filepath.Abs(testAgeKeyFilePath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("kustomize", "build", "--enable-alpha-plugins", "--enable-exec", sopsDir)
	cmd.Env = append(os.Environ(), "SOPS_AGE_KEY_FILE="+keyFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kustomize build: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "name: shop-api-key") || !strings.Contains(string(out), "hunter2") {
		t.Errorf("kustomize build output missing the decrypted Secret:\n%s", out)
	}
}
