package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/platform"
)

// testCapabilitiesPlatformYAML is testPlatformYAML plus the fields the
// Postgres and custom domain Capabilities need: a backups bucket and
// endpoint, and a Cloudflare zone one label wider than baseDomain so tests
// can tell "wildcard-covered", "in the zone but foreign" and "outside the
// zone" apart (docs/implementation-notes/13-cli-capabilities.md).
const testCapabilitiesPlatformYAML = testPlatformYAML + `cloudflareZone: itma.no
backupsBucket: itema-iidp-db-backups
objectStorageEndpoint: https://hel1.your-objectstorage.com
`

func TestAppCreatePostgresEnablesInEveryEnvironmentAndPropagatesBucket(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--postgres")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	for _, tc := range []struct {
		path []any
		want any
	}{
		{[]any{"postgres", "enabled"}, true},
		{[]any{"postgres", "migrationCommand"}, ""},
		{[]any{"platform", "backupsBucket"}, "itema-iidp-db-backups"},
		{[]any{"platform", "objectStorageEndpoint"}, "https://hel1.your-objectstorage.com"},
	} {
		if got := lookup(t, values, tc.path...); got != tc.want {
			t.Errorf("values.yaml %v = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestAppCreatePostgresRequiresBackupsBucketAndEndpoint(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML) // no backupsBucket/objectStorageEndpoint

	_, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--postgres")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "backupsBucket") || !strings.Contains(stderr, "objectStorageEndpoint") {
		t.Errorf("stderr = %q, want it to name the missing platform.yaml fields", stderr)
	}
	assertNoApplications(t, url)
}

func TestAppCreateMigrationCommandWithoutPostgresIsAnError(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--migration-command", "npm run migrate")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "--postgres") {
		t.Errorf("stderr = %q, want it to say --migration-command requires --postgres", stderr)
	}
	assertNoApplications(t, url)
}

func TestAppCreatePostgresRefusedOnStaticSite(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "static-site", "--postgres")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "static-site") && !strings.Contains(stderr, "Static site") {
		t.Errorf("stderr = %q, want it to explain a Static site cannot use Postgres", stderr)
	}
}

func TestAppCreateExplicitMigrationCommandOverridesDetection(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "prisma", "schema.prisma"), "// schema\n")

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--postgres",
		"--migration-command", "custom-migrate.sh", "--app-dir", dir)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if strings.Contains(stdout, "Detected") {
		t.Errorf("stdout = %q, want no detection message: the explicit flag must win outright", stdout)
	}
	// Without --path there is no Application repository to write iidp.yaml
	// into, so the command is printed as the line to add there; the
	// Platform's is left for the Deploy gate
	// (docs/implementation-notes/66-migration-command-in-repo.md).
	if !strings.Contains(stdout, "Add this line to iidp.yaml and push:\n  migrationCommand: custom-migrate.sh\n") {
		t.Errorf("stdout = %q, want the iidp.yaml line with the explicit command", stdout)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "postgres", "migrationCommand"); got != "" {
		t.Errorf("values.yaml postgres.migrationCommand = %v, want it left for the Deploy gate", got)
	}
}

func TestAppCreateRefusesAMigrationCommandThatIsNotOneLine(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--postgres", "--migration-command", "npm run migrate\nnpm run seed")

	if code == 0 || !strings.Contains(stderr, "must be one line") {
		t.Errorf("exit code = %d, stderr = %q, want a refusal saying it must be one line", code, stderr)
	}
	assertNoApplications(t, url)
}

func TestAppCreateDetectsMigrationTooling(t *testing.T) {
	for _, tc := range []struct {
		name        string
		write       func(t *testing.T, dir string)
		wantCommand string
		wantTool    string
	}{
		{
			name: "prisma",
			write: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "prisma", "schema.prisma"), "// schema\n")
			},
			wantCommand: "npx prisma migrate deploy",
			wantTool:    "Prisma",
		},
		{
			name: "drizzle",
			write: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "drizzle.config.ts"), "export default {}\n")
			},
			wantCommand: "npx drizzle-kit migrate",
			wantTool:    "Drizzle",
		},
		{
			name: "npm script",
			write: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "package.json"), `{"scripts":{"migrate":"node migrate.js"}}`)
			},
			wantCommand: "npm run migrate",
			wantTool:    "npm migrate script",
		},
		{
			name:        "none",
			write:       func(t *testing.T, dir string) {},
			wantCommand: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
			dir := t.TempDir()
			tc.write(t, dir)

			stdout, stderr, code := createApplication(t, url, cli.Dependencies{},
				"--name", "shop", "--kind", "web-service", "--postgres", "--app-dir", dir)

			if code != 0 {
				t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
			}
			values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
			if got := lookup(t, values, "postgres", "migrationCommand"); got != "" {
				t.Errorf("values.yaml postgres.migrationCommand = %v, want it left for the Deploy gate", got)
			}
			if tc.wantCommand != "" && !strings.Contains(stdout, "  migrationCommand: "+tc.wantCommand+"\n") {
				t.Errorf("stdout = %q, want the detected command as the iidp.yaml line to add", stdout)
			}
			if tc.wantTool != "" {
				if !strings.Contains(stdout, "Detected") || !strings.Contains(stdout, tc.wantTool) {
					t.Errorf("stdout = %q, want it to report detecting %s", stdout, tc.wantTool)
				}
			} else {
				if !strings.Contains(stdout, "No migration tooling detected") {
					t.Errorf("stdout = %q, want it to say nothing was detected", stdout)
				}
			}
		})
	}
}

func TestAppCreateStagingWritesASecondEnvironment(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--staging")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	for _, path := range []string{
		"applications/shop/prod/application.yaml",
		"applications/shop/prod/values.yaml",
		"applications/shop/staging/application.yaml",
		"applications/shop/staging/values.yaml",
	} {
		if _, err := os.Stat(filepath.Join(clone, path)); err != nil {
			t.Errorf("%s missing: %v", path, err)
		}
	}

	prodApp := readYAML(t, filepath.Join(clone, "applications/shop/prod/application.yaml"))
	stagingApp := readYAML(t, filepath.Join(clone, "applications/shop/staging/application.yaml"))
	if got := lookup(t, prodApp, "metadata", "name"); got != "shop-prod" {
		t.Errorf("prod ArgoCD Application name = %v, want shop-prod", got)
	}
	if got := lookup(t, stagingApp, "metadata", "name"); got != "shop-staging" {
		t.Errorf("staging ArgoCD Application name = %v, want shop-staging", got)
	}
	if got := lookup(t, stagingApp, "spec", "destination", "namespace"); got != "shop-staging" {
		t.Errorf("staging namespace = %v, want shop-staging", got)
	}
	// Both namespaces are Application namespaces, with their own
	// Environment's identity and the same Pod Security levels (#90).
	for _, env := range []struct {
		name string
		app  map[string]any
	}{{"prod", prodApp}, {"staging", stagingApp}} {
		labels, ok := lookup(t, env.app, "spec", "syncPolicy", "managedNamespaceMetadata", "labels").(map[string]any)
		if !ok {
			t.Fatalf("%s application.yaml has no managedNamespaceMetadata labels", env.name)
		}
		want := map[string]any{
			"iidp.itema.no/application":          "shop",
			"iidp.itema.no/environment":          env.name,
			"pod-security.kubernetes.io/enforce": "baseline",
			"pod-security.kubernetes.io/warn":    "restricted",
			"pod-security.kubernetes.io/audit":   "restricted",
		}
		if fmt.Sprint(labels) != fmt.Sprint(want) {
			t.Errorf("%s namespace labels = %v, want %v", env.name, labels, want)
		}
	}

	prodValues := readYAML(t, filepath.Join(clone, "applications/shop/prod/values.yaml"))
	stagingValues := readYAML(t, filepath.Join(clone, "applications/shop/staging/values.yaml"))
	if got := lookup(t, prodValues, "environment"); got != "prod" {
		t.Errorf("prod values.yaml environment = %v, want prod", got)
	}
	if got := lookup(t, stagingValues, "environment"); got != "staging" {
		t.Errorf("staging values.yaml environment = %v, want staging", got)
	}

	for _, want := range []string{"https://shop.app.itma.no", "https://shop-staging.app.itma.no"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

func TestAppCreateStagingGetsItsOwnPostgres(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--staging", "--postgres")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	for _, env := range []string{"prod", "staging"} {
		values := readYAML(t, filepath.Join(clone, "applications/shop", env, "values.yaml"))
		if got := lookup(t, values, "postgres", "enabled"); got != true {
			t.Errorf("%s values.yaml postgres.enabled = %v, want true", env, got)
		}
	}
}

func TestAppCreateCustomDomainWildcardCovered(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--domain", "butikk.app.itma.no")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "domains", 0); got != "butikk.app.itma.no" {
		t.Errorf("values.yaml domains[0] = %v, want butikk.app.itma.no", got)
	}
	if !strings.Contains(stdout, "butikk.app.itma.no") || !strings.Contains(stdout, "wildcard") {
		t.Errorf("stdout = %q, want it to say the wildcard certificate covers it", stdout)
	}
	if strings.Contains(stdout, "CNAME") {
		t.Errorf("stdout = %q, want no CNAME for a wildcard-covered domain", stdout)
	}
}

func TestAppCreateCustomDomainAutomaticInsideCloudflareZone(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--domain", "shop.itma.no")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "domains", 0); got != "shop.itma.no" {
		t.Errorf("values.yaml domains[0] = %v, want shop.itma.no", got)
	}
	if !strings.Contains(stdout, "shop.itma.no") || !strings.Contains(stdout, "automatic") {
		t.Errorf("stdout = %q, want it to say DNS is automatic inside the Cloudflare zone", stdout)
	}
	if strings.Contains(stdout, "CNAME") {
		t.Errorf("stdout = %q, want no CNAME for a domain inside the Cloudflare zone", stdout)
	}
}

func TestAppCreateCustomDomainOutsideZonePrintsCNAME(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--domain", "shop.example.com")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "domains", 0); got != "shop.example.com" {
		t.Errorf("values.yaml domains[0] = %v, want shop.example.com", got)
	}
	if !strings.Contains(stdout, "CNAME shop.example.com -> shop.app.itma.no") {
		t.Errorf("stdout = %q, want the CNAME to create", stdout)
	}
}

func TestAppCreateRefusesInvalidDomain(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--domain", "not_a_valid-host")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "not_a_valid-host") {
		t.Errorf("stderr = %q, want it to name the invalid domain", stderr)
	}
	assertNoApplications(t, url)
}

func TestAppCreateRefusesDuplicateDomain(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service",
		"--domain", "shop.example.com", "--domain", "shop.example.com")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "shop.example.com") || !strings.Contains(stderr, "twice") {
		t.Errorf("stderr = %q, want it to say the domain is listed twice", stderr)
	}
	assertNoApplications(t, url)
}

func TestAppCreateRefusesDomainEqualToAPlatformAddress(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	t.Run("prod's own address", func(t *testing.T) {
		_, stderr, code := createApplication(t, url, cli.Dependencies{},
			"--name", "shop", "--kind", "web-service", "--domain", "shop.app.itma.no")
		if code == 0 {
			t.Fatalf("exit code = 0, want non-zero")
		}
		if !strings.Contains(stderr, "shop.app.itma.no") {
			t.Errorf("stderr = %q, want it to name the domain", stderr)
		}
	})

	t.Run("staging's address", func(t *testing.T) {
		_, stderr, code := createApplication(t, url, cli.Dependencies{},
			"--name", "shop2", "--kind", "web-service", "--staging", "--domain", "shop2-staging.app.itma.no")
		if code == 0 {
			t.Fatalf("exit code = 0, want non-zero")
		}
		if !strings.Contains(stderr, "shop2-staging.app.itma.no") {
			t.Errorf("stderr = %q, want it to name the domain", stderr)
		}
	})
	assertNoApplications(t, url)
}

// deploy.<baseDomain> is the Deploy gate and auth.<baseDomain> the Itema
// login. An Application serving either address would share it with the
// Platform, and one serving the gate's could receive the OIDC tokens other
// Applications' workflows mint for it
// (docs/implementation-notes/60-deploy-gate.md).
func TestAppCreateRefusesThePlatformsOwnAddresses(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	for _, name := range []string{"deploy", "auth"} {
		_, stderr, code := createApplication(t, url, cli.Dependencies{}, "--name", name, "--kind", "web-service")
		if code == 0 || !strings.Contains(stderr, "reserved") {
			t.Errorf("--name %s: exit code = %d, stderr = %q, want it refused as reserved", name, code, stderr)
		}
		host := name + ".app.itma.no"
		_, stderr, code = createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service", "--domain", host)
		if code == 0 || !strings.Contains(stderr, host) || !strings.Contains(stderr, "Platform's own address") {
			t.Errorf("--domain %s: exit code = %d, stderr = %q, want it refused", host, code, stderr)
		}
	}
	assertNoApplications(t, url)
}

func TestAppCreateCustomDomainOnlyAppliesToProd(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--staging", "--domain", "shop.example.com")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	prodValues := readYAML(t, filepath.Join(clone, "applications/shop/prod/values.yaml"))
	stagingValues := readYAML(t, filepath.Join(clone, "applications/shop/staging/values.yaml"))
	if got := lookup(t, prodValues, "domains", 0); got != "shop.example.com" {
		t.Errorf("prod values.yaml domains[0] = %v, want shop.example.com", got)
	}
	if got, ok := lookup(t, stagingValues, "domains").([]any); !ok || len(got) != 0 {
		t.Errorf("staging values.yaml domains = %v, want empty: custom domains apply to prod only", lookup(t, stagingValues, "domains"))
	}
}

func TestAppCreateLoginEnablesInEveryEnvironment(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--staging", "--login")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	for _, env := range []string{"prod", "staging"} {
		values := readYAML(t, filepath.Join(clone, "applications/shop", env, "values.yaml"))
		if got := lookup(t, values, "login", "enabled"); got != true {
			t.Errorf("%s values.yaml login.enabled = %v, want true", env, got)
		}
		// The login cookie's domain, platform.yaml's cloudflareZone, which
		// the chart checks custom domains against (#76).
		if got := lookup(t, values, "platform", "loginCookieDomain"); got != "itma.no" {
			t.Errorf("%s values.yaml platform.loginCookieDomain = %v, want itma.no", env, got)
		}
	}
	if !strings.Contains(stdout, "Itema") {
		t.Errorf("stdout = %q, want it to mention Itema login", stdout)
	}
}

// Without --login nothing about the login cookie is written.
func TestAppCreateWithoutLoginWritesNoLoginCookieDomain(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--domain", "x.itma.no")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if platformValues := lookup(t, values, "platform").(map[string]any); platformValues["loginCookieDomain"] != nil {
		t.Errorf("platform.loginCookieDomain = %v, want it absent without --login", platformValues["loginCookieDomain"])
	}
}

// Custom domains inside cloudflareZone, the login cookie's domain, can be
// protected: one covered by the wildcard, and one only inside the zone.
func TestAppCreateLoginWithDomainsInsideTheZone(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--login", "--domain", "butikk.app.itma.no", "--domain", "x.itma.no")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "login", "enabled"); got != true {
		t.Errorf("login.enabled = %v, want true", got)
	}
	if got := lookup(t, values, "platform", "loginCookieDomain"); got != "itma.no" {
		t.Errorf("platform.loginCookieDomain = %v, want itma.no", got)
	}
	if got := fmt.Sprint(lookup(t, values, "domains")); got != "[butikk.app.itma.no x.itma.no]" {
		t.Errorf("domains = %s, want [butikk.app.itma.no x.itma.no]", got)
	}
}

// A domain outside the zone is refused, named, and nothing is written;
// the in-zone one given with it is not named.
func TestAppCreateLoginRefusedWithDomainOutsideTheZone(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--login", "--domain", "x.itma.no", "--domain", "shop.example.com")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "--login refused, custom domains outside itma.no: shop.example.com.") {
		t.Errorf("stderr = %q, want it to refuse --login naming shop.example.com as outside itma.no", stderr)
	}
	if strings.Contains(stderr, "x.itma.no") {
		t.Errorf("stderr = %q, want it not to name x.itma.no, which is inside the zone", stderr)
	}
	assertNoApplications(t, url)
}

// On the Create path the refusal comes before the Application repository
// is created: it needs platform.yaml, and the clone that checks the name
// has it.
func TestAppCreateLoginRefusedWithDomainOutsideTheZoneBeforeCreatingTheRepository(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)

	_, stderr, code := createApplication(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs", "--login", "--domain", "shop.example.com")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "shop.example.com") {
		t.Errorf("stderr = %q, want it to name shop.example.com", stderr)
	}
	if got := gh.cloneURL(platform.Org, "shop"); got != "" {
		t.Errorf("the Application repository was created (%s), want the refusal first", got)
	}
	assertNoApplications(t, url)
}

// Without a cloudflareZone the login cookie's domain is baseDomain, as it
// is for the bootstrap's oauth2-proxy: a custom domain under it is
// accepted, one only inside itma.no is not.
func TestAppCreateLoginCookieDomainIsBaseDomainWithoutAZone(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--login", "--domain", "x.itma.no")
	if code == 0 {
		t.Fatalf("x.itma.no: exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "outside app.itma.no: x.itma.no") {
		t.Errorf("stderr = %q, want it to name x.itma.no as outside app.itma.no", stderr)
	}
	assertNoApplications(t, url)

	_, stderr, code = createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--login", "--domain", "butikk.app.itma.no")
	if code != 0 {
		t.Fatalf("butikk.app.itma.no: exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "platform", "loginCookieDomain"); got != "app.itma.no" {
		t.Errorf("platform.loginCookieDomain = %v, want app.itma.no", got)
	}
}

func TestAppCreateAllCapabilitiesCombined(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--size", "medium",
		"--postgres", "--migration-command", "npm run migrate",
		"--staging", "--domain", "butikk.app.itma.no", "--domain", "shop.example.com")

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
		{prodValues, []any{"size"}, "medium"},
		{prodValues, []any{"postgres", "enabled"}, true},
		{prodValues, []any{"postgres", "migrationCommand"}, ""},
		{stagingValues, []any{"postgres", "enabled"}, true},
		{stagingValues, []any{"size"}, "medium"},
	} {
		if got := lookup(t, tc.doc, tc.path...); got != tc.want {
			t.Errorf("%v = %v, want %v", tc.path, got, tc.want)
		}
	}
	if got := lookup(t, prodValues, "domains", 0); got != "butikk.app.itma.no" {
		t.Errorf("prod domains[0] = %v", got)
	}
	if got := lookup(t, prodValues, "domains", 1); got != "shop.example.com" {
		t.Errorf("prod domains[1] = %v", got)
	}
	if got, ok := lookup(t, stagingValues, "domains").([]any); !ok || len(got) != 0 {
		t.Errorf("staging domains = %v, want empty", lookup(t, stagingValues, "domains"))
	}
	if !strings.Contains(stdout, "CNAME shop.example.com -> shop.app.itma.no") {
		t.Errorf("stdout = %q, want the CNAME line", stdout)
	}
}
