package render_test

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/render"
)

const testValuesYAML = `# Values for the prod Environment of shop. image.tag is written by the deploy workflow.
# Written by iidp; do not edit by hand.
application:
    name: shop
environment: prod
platform:
    baseDomain: app.itma.no
kind: web-service
image:
    repository: ghcr.io/itema-as/shop
    tag: ""
size: small
port: 3000
probe:
    path: /
env: {}
`

func TestAddSecretNameAddsAFreshList(t *testing.T) {
	out, changed, err := render.AddSecretName([]byte(testValuesYAML), "shop-api-key")
	if err != nil {
		t.Fatalf("AddSecretName: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if !strings.HasPrefix(string(out), "# Values for the prod Environment of shop") {
		t.Errorf("the header comment was lost:\n%s", out)
	}
	if !strings.Contains(string(out), "secrets:\n    - shop-api-key\n") {
		t.Errorf("secrets list missing or misshapen:\n%s", out)
	}
}

func TestAddSecretNameAppendsToAnExistingList(t *testing.T) {
	withOne, _, err := render.AddSecretName([]byte(testValuesYAML), "shop-api-key")
	if err != nil {
		t.Fatal(err)
	}

	out, changed, err := render.AddSecretName(withOne, "shop-db-password")
	if err != nil {
		t.Fatalf("AddSecretName: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if !strings.Contains(string(out), "secrets:\n    - shop-api-key\n    - shop-db-password\n") {
		t.Errorf("secrets list did not gain the second name:\n%s", out)
	}
}

func TestAddSecretNameIsIdempotent(t *testing.T) {
	withOne, _, err := render.AddSecretName([]byte(testValuesYAML), "shop-api-key")
	if err != nil {
		t.Fatal(err)
	}

	out, changed, err := render.AddSecretName(withOne, "shop-api-key")
	if err != nil {
		t.Fatalf("AddSecretName: %v", err)
	}
	if changed {
		t.Error("changed = true, want false: the name was already listed")
	}
	if string(out) != string(withOne) {
		t.Errorf("output changed even though changed=false:\nbefore:\n%s\nafter:\n%s", withOne, out)
	}
	if n := strings.Count(string(out), "shop-api-key"); n != 1 {
		t.Errorf("shop-api-key appears %d times in the secrets list, want 1", n)
	}
}

func TestEnablePostgresSetsEnabledAndPlatformFields(t *testing.T) {
	out, err := render.EnablePostgres([]byte(testValuesYAML), "itema-iidp-db-backups", "https://hel1.your-objectstorage.com")
	if err != nil {
		t.Fatalf("EnablePostgres: %v", err)
	}
	if !strings.HasPrefix(string(out), "# Values for the prod Environment of shop") {
		t.Errorf("the header comment was lost:\n%s", out)
	}
	for _, want := range []string{
		"postgres:\n    enabled: true\n",
		"platform:\n    baseDomain: app.itma.no\n    backupsBucket: itema-iidp-db-backups\n    objectStorageEndpoint: https://hel1.your-objectstorage.com\n",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// The migration command is the Deploy gate's to write, from the
	// Application repository's iidp.yaml.
	if strings.Contains(string(out), "migrationCommand") {
		t.Errorf("EnablePostgres set a migration command:\n%s", out)
	}
}

const postgresValuesYAML = `# shop's prod values
image:
    repository: ghcr.io/itema-as/shop
    tag: abc
postgres:
    enabled: true
    migrationCommand: ""
    backupRetention: 30d
`

func TestSetMigrationCommandSetsChangesAndClears(t *testing.T) {
	out, changed, err := render.SetMigrationCommand([]byte(postgresValuesYAML), "npx prisma migrate deploy && echo 'done: yes'")
	if err != nil || !changed {
		t.Fatalf("SetMigrationCommand: changed = %v, err = %v", changed, err)
	}
	if !strings.HasPrefix(string(out), "# shop's prod values") || !strings.Contains(string(out), "backupRetention: 30d") || !strings.Contains(string(out), "tag: abc") {
		t.Errorf("the rest of the document was not kept:\n%s", out)
	}
	if got := lookupValues(t, out, "postgres", "migrationCommand"); got != "npx prisma migrate deploy && echo 'done: yes'" {
		t.Errorf("migrationCommand = %v", got)
	}

	same, changed, err := render.SetMigrationCommand(out, "npx prisma migrate deploy && echo 'done: yes'")
	if err != nil || changed || string(same) != string(out) {
		t.Errorf("setting the same command: changed = %v, err = %v, want an untouched document", changed, err)
	}

	cleared, changed, err := render.SetMigrationCommand(out, "")
	if err != nil || !changed {
		t.Fatalf("clearing: changed = %v, err = %v", changed, err)
	}
	// Cleared to "", not to null: the value app create writes.
	if !strings.Contains(string(cleared), `migrationCommand: ""`) {
		t.Errorf("the cleared command is not \"\":\n%s", cleared)
	}
}

func TestSetMigrationCommandKeepsNumberLikeCommandsStrings(t *testing.T) {
	out, _, err := render.SetMigrationCommand([]byte(postgresValuesYAML), "true")
	if err != nil {
		t.Fatal(err)
	}
	if got := lookupValues(t, out, "postgres", "migrationCommand"); got != "true" {
		t.Errorf("migrationCommand = %#v, want the string \"true\"", got)
	}
}

func TestSetMigrationCommandRefusesAnEnvironmentWithoutPostgres(t *testing.T) {
	for name, values := range map[string]string{
		"no postgres block": testValuesYAML,
		"postgres off":      strings.Replace(postgresValuesYAML, "enabled: true", "enabled: false", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := render.SetMigrationCommand([]byte(values), "npm run migrate"); !errors.Is(err, render.ErrNoPostgres) {
				t.Errorf("err = %v, want ErrNoPostgres", err)
			}
			// Clearing what is not there changes nothing, and is not an error.
			out, changed, err := render.SetMigrationCommand([]byte(values), "")
			if err != nil || changed || string(out) != values {
				t.Errorf("clearing: changed = %v, err = %v, want an untouched document", changed, err)
			}
		})
	}
}

// lookupValues parses a values document and walks it by keys.
func lookupValues(t *testing.T, data []byte, path ...string) any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parsing:\n%s\n%v", data, err)
	}
	var cur any = doc
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%v: not a mapping at %q", path, key)
		}
		cur = m[key]
	}
	return cur
}

func TestEnablePostgresPreservesExistingSecretsList(t *testing.T) {
	withSecret, _, err := render.AddSecretName([]byte(testValuesYAML), "shop-api-key")
	if err != nil {
		t.Fatal(err)
	}
	out, err := render.EnablePostgres(withSecret, "bucket", "https://endpoint.example.com")
	if err != nil {
		t.Fatalf("EnablePostgres: %v", err)
	}
	if !strings.Contains(string(out), "secrets:\n    - shop-api-key\n") {
		t.Errorf("the secrets list was lost:\n%s", out)
	}
}

func TestEnableLoginSetsEnabledAndTheCookieDomain(t *testing.T) {
	out, err := render.EnableLogin([]byte(testValuesYAML), "itma.no")
	if err != nil {
		t.Fatalf("EnableLogin: %v", err)
	}
	if !strings.HasPrefix(string(out), "# Values for the prod Environment of shop") {
		t.Errorf("the header comment was lost:\n%s", out)
	}
	if !strings.Contains(string(out), "login:\n    enabled: true\n") {
		t.Errorf("login.enabled was not set:\n%s", out)
	}
	if !strings.Contains(string(out), "platform:\n    baseDomain: app.itma.no\n    loginCookieDomain: itma.no\n") {
		t.Errorf("platform.loginCookieDomain was not set next to baseDomain:\n%s", out)
	}
}

// Enabling again, as add-capability --domain does for an Environment that
// already has login, rewrites the cookie domain and leaves one of each key.
func TestEnableLoginAgainRewritesTheCookieDomain(t *testing.T) {
	first, err := render.EnableLogin([]byte(testValuesYAML), "app.itma.no")
	if err != nil {
		t.Fatal(err)
	}
	out, err := render.EnableLogin(first, "itma.no")
	if err != nil {
		t.Fatalf("EnableLogin: %v", err)
	}
	if got := strings.Count(string(out), "loginCookieDomain:"); got != 1 {
		t.Errorf("loginCookieDomain appears %d times, want 1:\n%s", got, out)
	}
	if !strings.Contains(string(out), "loginCookieDomain: itma.no\n") {
		t.Errorf("loginCookieDomain was not rewritten:\n%s", out)
	}
}

func TestEnableLoginPreservesExistingContent(t *testing.T) {
	withSecret, _, err := render.AddSecretName([]byte(testValuesYAML), "shop-api-key")
	if err != nil {
		t.Fatal(err)
	}
	out, err := render.EnableLogin(withSecret, "itma.no")
	if err != nil {
		t.Fatalf("EnableLogin: %v", err)
	}
	if !strings.Contains(string(out), "secrets:\n    - shop-api-key\n") {
		t.Errorf("the secrets list was lost:\n%s", out)
	}
}

const (
	groupA = "0f3b6a4e-8c1d-4e2f-9a7b-5c6d7e8f9a0b"
	groupB = "6e1d2c3b-4a5f-4b6c-8d7e-9f0a1b2c3d4e"
)

// Setting groups writes the whole list, and setting another list replaces
// it rather than adding to it: add-capability --login-group gives the new
// list in full.
func TestSetLoginGroupsWritesAndReplacesTheList(t *testing.T) {
	enabled, err := render.EnableLogin([]byte(testValuesYAML), "itma.no")
	if err != nil {
		t.Fatal(err)
	}
	withSecret, _, err := render.AddSecretName(enabled, "shop-api-key")
	if err != nil {
		t.Fatal(err)
	}
	first, changed, err := render.SetLoginGroups(withSecret, []string{groupA, groupB})
	if err != nil {
		t.Fatalf("SetLoginGroups: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if want := "login:\n    enabled: true\n    groups:\n        - " + groupA + "\n        - " + groupB + "\n"; !strings.Contains(string(first), want) {
		t.Errorf("login.groups not written as %q:\n%s", want, first)
	}
	second, changed, err := render.SetLoginGroups(first, []string{groupB})
	if err != nil {
		t.Fatalf("SetLoginGroups: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if strings.Contains(string(second), groupA) || !strings.Contains(string(second), "    groups:\n        - "+groupB+"\n") {
		t.Errorf("login.groups was not replaced by [%s]:\n%s", groupB, second)
	}
	for _, want := range []string{"# Values for the prod Environment of shop", "secrets:\n    - shop-api-key\n", "loginCookieDomain: itma.no\n"} {
		if !strings.Contains(string(second), want) {
			t.Errorf("lost %q:\n%s", want, second)
		}
	}
}

// An empty list clears the groups, written as groups: [] the way app
// create writes it; the same list again changes nothing.
func TestSetLoginGroupsClearsAndReportsNoChange(t *testing.T) {
	with, _, err := render.SetLoginGroups([]byte(testValuesYAML), []string{groupA})
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := render.SetLoginGroups(with, []string{groupA}); err != nil || changed {
		t.Errorf("the same list again: changed = %v, err = %v, want no change", changed, err)
	}
	cleared, changed, err := render.SetLoginGroups(with, nil)
	if err != nil {
		t.Fatalf("SetLoginGroups: %v", err)
	}
	if !changed || !strings.Contains(string(cleared), "login:\n    groups: []\n") || strings.Contains(string(cleared), groupA) {
		t.Errorf("changed = %v, want the groups cleared to []:\n%s", changed, cleared)
	}
	if _, changed, err := render.SetLoginGroups(cleared, []string{}); err != nil || changed {
		t.Errorf("clearing again: changed = %v, err = %v, want no change", changed, err)
	}
}

// A new staging Environment takes prod's groups with the rest of prod's
// values.
func TestCopyValuesForStagingKeepsTheLoginGroups(t *testing.T) {
	with, _, err := render.SetLoginGroups([]byte(testValuesYAML), []string{groupA})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := render.CopyValuesForStaging(with)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "    groups:\n        - "+groupA+"\n") {
		t.Errorf("staging lost the login groups:\n%s", out)
	}
}

func TestSetSizeChangesTheSizeKey(t *testing.T) {
	out, changed, err := render.SetSize([]byte(testValuesYAML), "medium")
	if err != nil {
		t.Fatalf("SetSize: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if !strings.Contains(string(out), "size: medium\n") {
		t.Errorf("size was not updated:\n%s", out)
	}
	if strings.Contains(string(out), "size: small") {
		t.Errorf("the old size is still present:\n%s", out)
	}
}

func TestSetSizeIsIdempotent(t *testing.T) {
	out, changed, err := render.SetSize([]byte(testValuesYAML), "small")
	if err != nil {
		t.Fatalf("SetSize: %v", err)
	}
	if changed {
		t.Error("changed = true, want false: the size was already small")
	}
	if string(out) != testValuesYAML {
		t.Errorf("output changed even though changed=false:\nbefore:\n%s\nafter:\n%s", testValuesYAML, out)
	}
}

func TestAddDomainAppendsAndIsIdempotent(t *testing.T) {
	withOne, changed, err := render.AddDomain([]byte(testValuesYAML), "shop.example.com")
	if err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if !strings.Contains(string(withOne), "domains:\n    - shop.example.com\n") {
		t.Errorf("domains list missing or misshapen:\n%s", withOne)
	}

	out, changed, err := render.AddDomain(withOne, "shop.example.com")
	if err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	if changed {
		t.Error("changed = true, want false: the domain was already listed")
	}
	if string(out) != string(withOne) {
		t.Errorf("output changed even though changed=false")
	}
}

func TestCopyValuesForStagingSetsEnvironmentClearsDomainsTagAndSecrets(t *testing.T) {
	withDomain, _, err := render.AddDomain([]byte(testValuesYAML), "shop.example.com")
	if err != nil {
		t.Fatal(err)
	}
	withSecret, _, err := render.AddSecretName(withDomain, "shop-api-key")
	if err != nil {
		t.Fatal(err)
	}

	out, secretsDropped, err := render.CopyValuesForStaging(withSecret)
	if err != nil {
		t.Fatalf("CopyValuesForStaging: %v", err)
	}
	if !secretsDropped {
		t.Error("secretsDropped = false, want true: prod had a secrets list")
	}
	if !strings.Contains(string(out), "environment: staging\n") {
		t.Errorf("environment was not set to staging:\n%s", out)
	}
	if strings.Contains(string(out), "shop.example.com") {
		t.Errorf("the custom domain was copied to staging:\n%s", out)
	}
	if strings.Contains(string(out), "shop-api-key") || strings.Contains(string(out), "secrets:") {
		t.Errorf("prod's secrets were copied to staging:\n%s", out)
	}
	if !strings.Contains(string(out), `tag: ""`) {
		t.Errorf("image.tag should be reset to empty for a fresh staging Environment:\n%s", out)
	}
	// Everything else must survive the copy.
	if !strings.Contains(string(out), "repository: ghcr.io/itema-as/shop") {
		t.Errorf("image.repository was lost:\n%s", out)
	}
}

func TestCopyValuesForStagingReportsNoSecretsDropped(t *testing.T) {
	_, secretsDropped, err := render.CopyValuesForStaging([]byte(testValuesYAML))
	if err != nil {
		t.Fatal(err)
	}
	if secretsDropped {
		t.Error("secretsDropped = true, want false: prod had no secrets")
	}
}

const testApplicationYAML = `# The ArgoCD Application for the prod Environment of shop.
# Written by iidp; do not edit by hand.
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
    name: shop-prod
    namespace: argocd
    labels:
        iidp.itema.no/application: shop
        iidp.itema.no/environment: prod
    finalizers:
        - resources-finalizer.argocd.argoproj.io
spec:
    project: default
    sources:
        - repoURL: ghcr.io/itema-as/charts
          chart: application
          targetRevision: 0.3.1
          helm:
            valueFiles:
                - $values/applications/shop/prod/values.yaml
        - repoURL: https://github.com/Itema-as/iidp-platform.git
          targetRevision: main
          ref: values
    destination:
        server: https://kubernetes.default.svc
        namespace: shop-prod
    syncPolicy:
        automated:
            prune: true
            selfHeal: true
        syncOptions:
            - CreateNamespace=true
`

func TestAddKustomizeSourceAddsAThirdSource(t *testing.T) {
	out, changed, err := render.AddKustomizeSource([]byte(testApplicationYAML),
		"https://github.com/Itema-as/iidp-platform.git", "applications/shop/prod/sops")
	if err != nil {
		t.Fatalf("AddKustomizeSource: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if !strings.HasPrefix(string(out), "# The ArgoCD Application for the prod Environment of shop.") {
		t.Errorf("the header comment was lost:\n%s", out)
	}
	want := "        - repoURL: https://github.com/Itema-as/iidp-platform.git\n" +
		"          targetRevision: main\n" +
		"          path: applications/shop/prod/sops\n"
	if !strings.Contains(string(out), want) {
		t.Errorf("output lacks the third source %q:\n%s", want, out)
	}
	// The existing two sources must be untouched.
	if !strings.Contains(string(out), "ref: values") {
		t.Errorf("the values source was lost:\n%s", out)
	}
}

func TestAddKustomizeSourceIsIdempotent(t *testing.T) {
	withOne, _, err := render.AddKustomizeSource([]byte(testApplicationYAML),
		"https://github.com/Itema-as/iidp-platform.git", "applications/shop/prod/sops")
	if err != nil {
		t.Fatal(err)
	}

	out, changed, err := render.AddKustomizeSource(withOne,
		"https://github.com/Itema-as/iidp-platform.git", "applications/shop/prod/sops")
	if err != nil {
		t.Fatalf("AddKustomizeSource: %v", err)
	}
	if changed {
		t.Error("changed = true, want false: the source was already there")
	}
	if string(out) != string(withOne) {
		t.Errorf("output changed even though changed=false:\nbefore:\n%s\nafter:\n%s", withOne, out)
	}
	if n := strings.Count(string(out), "applications/shop/prod/sops"); n != 1 {
		t.Errorf("the sops path appears %d times, want 1", n)
	}
}

func TestSetImageTagSetsAnEmptyTag(t *testing.T) {
	out, changed, err := render.SetImageTag([]byte(testValuesYAML), "a1b2c3d4")
	if err != nil {
		t.Fatalf("SetImageTag: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if !strings.HasPrefix(string(out), "# Values for the prod Environment of shop") {
		t.Errorf("the header comment was lost:\n%s", out)
	}
	if !strings.Contains(string(out), "image:\n    repository: ghcr.io/itema-as/shop\n    tag: a1b2c3d4\n") {
		t.Errorf("image.tag was not set:\n%s", out)
	}
	// Everything else, including comments and the rest of image:, survives.
	if !strings.Contains(string(out), "size: small\n") || !strings.Contains(string(out), "port: 3000\n") {
		t.Errorf("unrelated keys were lost:\n%s", out)
	}
}

func TestSetImageTagReplacesAnExistingTag(t *testing.T) {
	withOne, _, err := render.SetImageTag([]byte(testValuesYAML), "a1b2c3d4")
	if err != nil {
		t.Fatal(err)
	}

	out, changed, err := render.SetImageTag(withOne, "1.2.3")
	if err != nil {
		t.Fatalf("SetImageTag: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if !strings.Contains(string(out), "tag: 1.2.3\n") {
		t.Errorf("image.tag was not replaced:\n%s", out)
	}
	if strings.Contains(string(out), "a1b2c3d4") {
		t.Errorf("the old tag is still present:\n%s", out)
	}
}

func TestSetImageTagIsIdempotent(t *testing.T) {
	withOne, _, err := render.SetImageTag([]byte(testValuesYAML), "a1b2c3d4")
	if err != nil {
		t.Fatal(err)
	}

	out, changed, err := render.SetImageTag(withOne, "a1b2c3d4")
	if err != nil {
		t.Fatalf("SetImageTag: %v", err)
	}
	if changed {
		t.Error("changed = true, want false: the tag was already set")
	}
	if string(out) != string(withOne) {
		t.Errorf("output changed even though changed=false:\nbefore:\n%s\nafter:\n%s", withOne, out)
	}
}

func TestSetImageTagRejectsAValuesFileWithoutImage(t *testing.T) {
	_, _, err := render.SetImageTag([]byte("application:\n    name: shop\n"), "a1b2c3d4")
	if err == nil {
		t.Fatal("err = nil, want an error: no image key")
	}
}
