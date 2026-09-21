package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/cli"
)

// seedApplication creates "shop" on the Platform repository at url through
// iidp app create, so add-capability tests start from an Application that
// already exists, the way #13's Capability tests seed nothing further.
func seedApplication(t *testing.T, url string, extraCreateArgs ...string) {
	t.Helper()
	args := append([]string{"--name", "shop", "--kind", "web-service"}, extraCreateArgs...)
	_, stderr, code := createApplication(t, url, cli.Dependencies{}, args...)
	if code != 0 {
		t.Fatalf("seeding shop: exit code = %d, want 0\nstderr: %s", code, stderr)
	}
}

// addCapability runs iidp app add-capability in-process against the
// Platform repository at url.
func addCapability(t *testing.T, url, name string, deps cli.Dependencies, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if deps.TokenSource == nil {
		deps.TokenSource = fakeTokenSource{token: "gho_test"}
	}
	full := append([]string{"app", "add-capability", name, "--platform-repo", url}, args...)
	var out, errOut bytes.Buffer
	code = cli.RunWith(full, strings.NewReader(""), &out, &errOut, deps)
	return out.String(), errOut.String(), code
}

func TestAppAddCapabilityRequiresAtLeastOneFlag(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)

	_, stderr, code := addCapability(t, url, "shop", cli.Dependencies{})

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "at least one") {
		t.Errorf("stderr = %q, want it to say at least one Capability flag is required", stderr)
	}
}

func TestAppAddCapabilityExplicitFalseFlagStillRequiresACapability(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)

	// --staging=false is "changed" from cobra's point of view but asks for
	// nothing; it must not slip past the "at least one flag" gate and reach
	// AddCapabilities with an all-zero request.
	_, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--staging=false")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "at least one") {
		t.Errorf("stderr = %q, want it to say at least one Capability flag is required", stderr)
	}
	if got := headSubject(t, url); got != "iidp app create shop" {
		t.Errorf("Platform repository head is %q, want unchanged (no empty commit)", got)
	}
}

func TestAppAddCapabilityRefusesUnknownApplication(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	_, stderr, code := addCapability(t, url, "ghost", cli.Dependencies{}, "--postgres")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "ghost") {
		t.Errorf("stderr = %q, want it to name the missing Application", stderr)
	}
}

func TestAppAddCapabilityPostgresEnablesInEveryEnvironment(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--staging")

	stdout, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--postgres")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	for _, env := range []string{"prod", "staging"} {
		values := readYAML(t, filepath.Join(clone, "applications/shop", env, "values.yaml"))
		for _, tc := range []struct {
			path []any
			want any
		}{
			{[]any{"postgres", "enabled"}, true},
			{[]any{"platform", "backupsBucket"}, "itema-iidp-db-backups"},
			{[]any{"platform", "objectStorageEndpoint"}, "https://hel1.your-objectstorage.com"},
		} {
			if got := lookup(t, values, tc.path...); got != tc.want {
				t.Errorf("%s values.yaml %v = %v, want %v", env, tc.path, got, tc.want)
			}
		}
	}
	if got := strings.TrimSpace(gitRun(t, clone, "log", "-1", "--format=%s")); got != "iidp app add-capability shop postgres" {
		t.Errorf("commit subject = %q, want %q", got, "iidp app add-capability shop postgres")
	}
	if !strings.Contains(stdout, "https://shop.app.itma.no") || !strings.Contains(stdout, "https://shop-staging.app.itma.no") {
		t.Errorf("stdout = %q, want both addresses", stdout)
	}
}

func TestAppAddCapabilityPostgresPreservesSecretsAndComments(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)

	// Stand in for iidp secret set having already run: a secrets list
	// added by hand, the way a real Environment would carry one before
	// add-capability touches it.
	clone := cloneMain(t, url)
	valuesPath := filepath.Join(clone, "applications/shop/prod/values.yaml")
	original, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(original), "# Values for the prod Environment of shop") {
		t.Fatalf("fixture assumption broken, no header comment:\n%s", original)
	}
	handEdited := string(original) + "secrets:\n    - shop-api-key\n"
	pushCommit(t, url, "iidp secret set shop prod API_KEY", map[string]string{
		"applications/shop/prod/values.yaml": handEdited,
	})

	_, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--postgres")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "# Values for the prod Environment of shop") {
		t.Errorf("the header comment was lost:\n%s", data)
	}
	if !strings.Contains(string(data), "secrets:\n    - shop-api-key\n") {
		t.Errorf("the secrets list was lost:\n%s", data)
	}
	if !strings.Contains(string(data), "enabled: true") {
		t.Errorf("postgres was not enabled:\n%s", data)
	}
}

func TestAppAddCapabilityRefusesPostgresAlreadyEnabled(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--postgres")

	_, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--postgres")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "Postgres") || !strings.Contains(stderr, "already") {
		t.Errorf("stderr = %q, want it to say Postgres is already enabled", stderr)
	}
}

func TestAppAddCapabilityStagingCopiesProdValues(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--postgres", "--size", "medium")

	stdout, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--staging")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	for _, path := range []string{"applications/shop/staging/application.yaml", "applications/shop/staging/values.yaml"} {
		if _, err := os.Stat(filepath.Join(clone, path)); err != nil {
			t.Errorf("%s missing: %v", path, err)
		}
	}
	stagingValues := readYAML(t, filepath.Join(clone, "applications/shop/staging/values.yaml"))
	for _, tc := range []struct {
		path []any
		want any
	}{
		{[]any{"environment"}, "staging"},
		{[]any{"size"}, "medium"},
		{[]any{"postgres", "enabled"}, true},
		{[]any{"image", "tag"}, ""},
	} {
		if got := lookup(t, stagingValues, tc.path...); got != tc.want {
			t.Errorf("staging values.yaml %v = %v, want %v", tc.path, got, tc.want)
		}
	}
	stagingApp := readYAML(t, filepath.Join(clone, "applications/shop/staging/application.yaml"))
	if got := lookup(t, stagingApp, "metadata", "name"); got != "shop-staging" {
		t.Errorf("staging ArgoCD Application name = %v, want shop-staging", got)
	}
	if !strings.Contains(stdout, "https://shop-staging.app.itma.no") {
		t.Errorf("stdout = %q, want the staging address", stdout)
	}
}

func TestAppAddCapabilityStagingLogsThatSecretsAreNotCopied(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)
	clone := cloneMain(t, url)
	original, err := os.ReadFile(filepath.Join(clone, "applications/shop/prod/values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	pushCommit(t, url, "iidp secret set shop prod API_KEY", map[string]string{
		"applications/shop/prod/values.yaml": string(original) + "secrets:\n    - shop-api-key\n",
	})

	stdout, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--staging")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "not copied") {
		t.Errorf("stdout = %q, want it to say prod's secrets are not copied", stdout)
	}
	stagingValues := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/staging/values.yaml"))
	if _, ok := stagingValues["secrets"]; ok {
		t.Errorf("staging values.yaml has a secrets key, want none: %v", stagingValues["secrets"])
	}
}

func TestAppAddCapabilityRefusesStagingAlreadyExists(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--staging")

	_, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--staging")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "shop") || !strings.Contains(stderr, "staging") {
		t.Errorf("stderr = %q, want it to name the Application and staging", stderr)
	}
}

func TestAppAddCapabilityDomainAppliesToProdOnly(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--staging")

	stdout, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--domain", "butikk.app.itma.no")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	prodValues := readYAML(t, filepath.Join(clone, "applications/shop/prod/values.yaml"))
	stagingValues := readYAML(t, filepath.Join(clone, "applications/shop/staging/values.yaml"))
	if got := lookup(t, prodValues, "domains", 0); got != "butikk.app.itma.no" {
		t.Errorf("prod domains[0] = %v, want butikk.app.itma.no", got)
	}
	if got, ok := lookup(t, stagingValues, "domains").([]any); !ok || len(got) != 0 {
		t.Errorf("staging domains = %v, want empty", lookup(t, stagingValues, "domains"))
	}
	if !strings.Contains(stdout, "butikk.app.itma.no") || !strings.Contains(stdout, "wildcard") {
		t.Errorf("stdout = %q, want it to classify the domain like app create does", stdout)
	}
}

func TestAppAddCapabilityRefusesDomainAlreadyListed(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--domain", "shop.example.com")

	_, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--domain", "shop.example.com")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "shop.example.com") || !strings.Contains(stderr, "already") {
		t.Errorf("stderr = %q, want it to name the domain as already listed", stderr)
	}
}

func TestAppAddCapabilitySizeRewritesEveryEnvironment(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--staging")

	_, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--size", "large")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	for _, env := range []string{"prod", "staging"} {
		values := readYAML(t, filepath.Join(clone, "applications/shop", env, "values.yaml"))
		if got := lookup(t, values, "size"); got != "large" {
			t.Errorf("%s size = %v, want large", env, got)
		}
	}
}

func TestAppAddCapabilityRefusesSameSize(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url) // default size: small

	_, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--size", "small")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "small") || !strings.Contains(stderr, "already") {
		t.Errorf("stderr = %q, want it to say size is already small", stderr)
	}
}

func TestAppAddCapabilityDetectsMigrationCommand(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "prisma", "schema.prisma"), "// schema\n")

	stdout, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--postgres", "--app-dir", dir)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Detected") || !strings.Contains(stdout, "Prisma") {
		t.Errorf("stdout = %q, want it to report detecting Prisma", stdout)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "postgres", "migrationCommand"); got != "npx prisma migrate deploy" {
		t.Errorf("migrationCommand = %v, want the detected Prisma command", got)
	}
}

func TestAppAddCapabilityMigrationCommandWithoutPostgresIsAnError(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)

	_, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--migration-command", "npm run migrate")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "--postgres") {
		t.Errorf("stderr = %q, want it to say --migration-command requires --postgres", stderr)
	}
}

func TestAppAddCapabilityAllCapabilitiesCombined(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)

	stdout, stderr, code := addCapability(t, url, "shop", cli.Dependencies{},
		"--postgres", "--migration-command", "npm run migrate",
		"--staging", "--domain", "butikk.app.itma.no", "--size", "medium")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	prodValues := readYAML(t, filepath.Join(clone, "applications/shop/prod/values.yaml"))
	stagingValues := readYAML(t, filepath.Join(clone, "applications/shop/staging/values.yaml"))
	for _, tc := range []struct {
		doc  any
		path []any
		want any
	}{
		{prodValues, []any{"postgres", "enabled"}, true},
		{prodValues, []any{"postgres", "migrationCommand"}, "npm run migrate"},
		{prodValues, []any{"size"}, "medium"},
		{prodValues, []any{"domains", 0}, "butikk.app.itma.no"},
		{stagingValues, []any{"postgres", "enabled"}, true},
		{stagingValues, []any{"size"}, "medium"},
	} {
		if got := lookup(t, tc.doc, tc.path...); got != tc.want {
			t.Errorf("%v = %v, want %v", tc.path, got, tc.want)
		}
	}
	if got := strings.TrimSpace(gitRun(t, clone, "log", "-1", "--format=%s")); got != "iidp app add-capability shop postgres staging domain size" {
		t.Errorf("commit subject = %q", got)
	}
	if !strings.Contains(stdout, "https://shop-staging.app.itma.no") {
		t.Errorf("stdout = %q, want the staging address", stdout)
	}
}
