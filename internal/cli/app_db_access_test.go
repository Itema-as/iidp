package cli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/sops"
)

// fakeEncryptor stands in for sops, so the tests that write passwords need
// no binary: it wraps the plaintext in base64 under a fakeSops key, naming
// the recipient. decryptFake undoes it.
type fakeEncryptor struct{}

func (fakeEncryptor) Encrypt(_ context.Context, plaintext []byte, recipient, _ string) ([]byte, error) {
	return yaml.Marshal(map[string]string{"recipient": recipient, "fakeSops": base64.StdEncoding.EncodeToString(plaintext)})
}

// decryptFake reads a file fakeEncryptor wrote and returns the Secret
// document inside.
func decryptFake(t *testing.T, path string) map[string]any {
	t.Helper()
	wrapper := readYAML(t, path)
	if wrapper["recipient"] != testAgePublicKey {
		t.Errorf("%s is encrypted for %v, want the Platform's age key", path, wrapper["recipient"])
	}
	plaintext, err := base64.StdEncoding.DecodeString(wrapper["fakeSops"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(plaintext, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func dbAccess(t *testing.T, url string, deps cli.Dependencies, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if deps.TokenSource == nil {
		deps.TokenSource = fakeTokenSource{token: "gho_test"}
	}
	if deps.Encryptor == nil {
		deps.Encryptor = fakeEncryptor{}
	}
	full := append([]string{"app", "db", "access", "--platform-repo", url}, args...)
	var out, errOut bytes.Buffer
	code = cli.RunWith(full, strings.NewReader(""), &out, &errOut, deps)
	return out.String(), errOut.String(), code
}

// seedExistingDatabase creates shop with Postgres in prod and staging the
// way an Application from before database access is: without passwords,
// since platform.yaml had no age key. It then adds the key, so passwords
// can be written.
func seedExistingDatabase(t *testing.T, extraCreateArgs ...string) string {
	t.Helper()
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	args := append([]string{"--name", "shop", "--kind", "web-service", "--postgres", "--staging"}, extraCreateArgs...)
	if _, stderr, code := createApplication(t, url, cli.Dependencies{}, args...); code != 0 {
		t.Fatalf("seeding shop: exit %d\n%s", code, stderr)
	}
	pushCommit(t, url, "Add the age key", map[string]string{"platform.yaml": testCapabilitiesPlatformYAML + "agePublicKey: " + testAgePublicKey + "\n"})
	return url
}

// postgresValues returns the postgres block of an Environment's values.yaml.
func postgresValues(t *testing.T, clone, environment string) map[string]any {
	t.Helper()
	return lookup(t, readYAML(t, filepath.Join(clone, "applications", "shop", environment, "values.yaml")), "postgres").(map[string]any)
}

// ksopsFiles lists the files an Environment's ksops.yaml decrypts.
func ksopsFiles(t *testing.T, clone, environment string) []string {
	t.Helper()
	var files []string
	for _, f := range lookup(t, readYAML(t, filepath.Join(clone, "applications", "shop", environment, "sops", "ksops.yaml")), "files").([]any) {
		files = append(files, f.(string))
	}
	return files
}

// assertPasswordSecret checks the password Secret at path: basic-auth for
// the role, with a generated password.
func assertPasswordSecret(t *testing.T, path, name, environment, role string) {
	t.Helper()
	doc := decryptFake(t, path)
	if doc["type"] != "kubernetes.io/basic-auth" {
		t.Errorf("%s: type = %v, want kubernetes.io/basic-auth", path, doc["type"])
	}
	metadata := doc["metadata"].(map[string]any)
	if metadata["name"] != name {
		t.Errorf("%s: name = %v, want %s", path, metadata["name"], name)
	}
	if env := metadata["labels"].(map[string]any)["iidp.itema.no/environment"]; env != environment {
		t.Errorf("%s: environment label = %v, want %s", path, env, environment)
	}
	data := doc["stringData"].(map[string]any)
	if data["username"] != role {
		t.Errorf("%s: username = %v, want %s", path, data["username"], role)
	}
	if password, _ := data["password"].(string); len(password) < 24 {
		t.Errorf("%s: password %q is not a generated one", path, password)
	}
}

func TestDBAccessWithoutLevelsWritesThePasswordsOfTheCurrentLevels(t *testing.T) {
	url := seedExistingDatabase(t)

	stdout, stderr, code := dbAccess(t, url, cli.Dependencies{}, "shop", "--env", "staging")
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "staging database: read-write push · read-only none") {
		t.Errorf("stdout does not print the levels:\n%s", stdout)
	}
	if got := headSubject(t, url); got != "iidp app db access shop --env staging --read-write push --read-only none" {
		t.Errorf("commit subject = %q", got)
	}

	clone := cloneMain(t, url)
	postgres := postgresValues(t, clone, "staging")
	if access := postgres["access"].(map[string]any); access["readWrite"] != "push" || access["readOnly"] != "none" {
		t.Errorf("postgres.access = %v, want staging's defaults written", access)
	}
	if postgres["readWritePasswordSecret"] != "shop-staging-db-write" {
		t.Errorf("readWritePasswordSecret = %v, want shop-staging-db-write", postgres["readWritePasswordSecret"])
	}
	if _, named := postgres["readOnlyPasswordSecret"]; named {
		t.Errorf("readOnlyPasswordSecret is named for a level of none: %v", postgres)
	}
	sops := filepath.Join(clone, "applications", "shop", "staging", "sops")
	assertPasswordSecret(t, filepath.Join(sops, "db-write.enc.yaml"), "shop-staging-db-write", "staging", "shop_write")
	if _, err := os.Stat(filepath.Join(sops, "db-read.enc.yaml")); err == nil {
		t.Error("wrote a read-only password for a level of none")
	}
	if files := ksopsFiles(t, clone, "staging"); !slices.Equal(files, []string{"backups-credentials.enc.yaml", "db-write.enc.yaml"}) {
		t.Errorf("ksops.yaml files = %v", files)
	}
	assertOneSopsSource(t, filepath.Join(clone, "applications", "shop", "staging", "application.yaml"), "shop", "staging")

	// prod is closed by default: its levels are written, and no password.
	if _, stderr, code := dbAccess(t, url, cli.Dependencies{}, "shop", "--env", "prod"); code != 0 {
		t.Fatalf("prod: exit %d\n%s", code, stderr)
	}
	clone = cloneMain(t, url)
	if access := postgresValues(t, clone, "prod")["access"].(map[string]any); access["readWrite"] != "none" || access["readOnly"] != "none" {
		t.Errorf("prod postgres.access = %v, want none and none", access)
	}
	if files := ksopsFiles(t, clone, "prod"); !slices.Equal(files, []string{"backups-credentials.enc.yaml"}) {
		t.Errorf("prod ksops.yaml files = %v, want no password", files)
	}
}

func TestDBAccessGeneratesTheMissingPasswordOfANewLevel(t *testing.T) {
	url := seedExistingDatabase(t)

	stdout, stderr, code := dbAccess(t, url, cli.Dependencies{}, "shop", "--env", "prod", "--read-only", "maintain")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "prod database: read-write none · read-only maintain") {
		t.Errorf("stdout does not print the levels:\n%s", stdout)
	}
	clone := cloneMain(t, url)
	postgres := postgresValues(t, clone, "prod")
	if access := postgres["access"].(map[string]any); access["readWrite"] != "none" || access["readOnly"] != "maintain" {
		t.Errorf("postgres.access = %v", access)
	}
	if postgres["readOnlyPasswordSecret"] != "shop-db-read" {
		t.Errorf("readOnlyPasswordSecret = %v, want shop-db-read", postgres["readOnlyPasswordSecret"])
	}
	assertPasswordSecret(t, filepath.Join(clone, "applications", "shop", "prod", "sops", "db-read.enc.yaml"), "shop-db-read", "prod", "shop_read")
}

func TestDBAccessKeepsAPasswordAndCommitsNothingWhenNothingChanges(t *testing.T) {
	url := seedExistingDatabase(t)
	if _, stderr, code := dbAccess(t, url, cli.Dependencies{}, "shop", "--env", "staging"); code != 0 {
		t.Fatalf("first run: exit %d\n%s", code, stderr)
	}
	before := readFile(t, filepath.Join(cloneMain(t, url), "applications", "shop", "staging", "sops", "db-write.enc.yaml"))
	head := gitRun(t, cloneMain(t, url), "rev-parse", "HEAD")

	stdout, stderr, code := dbAccess(t, url, cli.Dependencies{}, "shop", "--env", "staging", "--read-write", "push")
	if code != 0 {
		t.Fatalf("second run: exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "Nothing to change") || !strings.Contains(stdout, "staging database: read-write push · read-only none") {
		t.Errorf("stdout = %s, want nothing to change and the levels", stdout)
	}
	clone := cloneMain(t, url)
	if got := gitRun(t, clone, "rev-parse", "HEAD"); got != head {
		t.Error("a run that changed nothing committed")
	}
	if after := readFile(t, filepath.Join(clone, "applications", "shop", "staging", "sops", "db-write.enc.yaml")); after != before {
		t.Error("the existing password was replaced")
	}
}

func TestDBAccessRemovesThePasswordOfALevelThatBecameNone(t *testing.T) {
	url := seedExistingDatabase(t)
	if _, stderr, code := dbAccess(t, url, cli.Dependencies{}, "shop", "--env", "staging", "--read-only", "pull"); code != 0 {
		t.Fatalf("opening read-only: exit %d\n%s", code, stderr)
	}

	stdout, stderr, code := dbAccess(t, url, cli.Dependencies{}, "shop", "--env", "staging", "--read-write", "none")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "staging database: read-write none · read-only pull") {
		t.Errorf("stdout does not print the levels:\n%s", stdout)
	}
	clone := cloneMain(t, url)
	postgres := postgresValues(t, clone, "staging")
	if _, named := postgres["readWritePasswordSecret"]; named {
		t.Errorf("readWritePasswordSecret is still named: %v", postgres)
	}
	if postgres["readOnlyPasswordSecret"] != "shop-staging-db-read" {
		t.Errorf("readOnlyPasswordSecret = %v, want it kept", postgres["readOnlyPasswordSecret"])
	}
	if _, err := os.Stat(filepath.Join(clone, "applications", "shop", "staging", "sops", "db-write.enc.yaml")); err == nil {
		t.Error("db-write.enc.yaml is still committed")
	}
	if files := ksopsFiles(t, clone, "staging"); !slices.Equal(files, []string{"backups-credentials.enc.yaml", "db-read.enc.yaml"}) {
		t.Errorf("ksops.yaml files = %v", files)
	}
}

func TestDBAccessRefusesWhatTheChartRefusesWithItsMessage(t *testing.T) {
	url := seedExistingDatabase(t)
	noPostgres := newPlatformRepository(t, testCapabilitiesPlatformYAML+"agePublicKey: "+testAgePublicKey+"\n")
	createShopApplication(t, noPostgres)
	for _, tc := range []struct {
		url     string
		args    []string
		message string
	}{
		// The messages are the chart's (chart/application/database_access_test.go).
		{url, []string{"--env", "staging", "--read-write", "triage"}, `postgres.access.readWrite must be none, pull, push, maintain or admin, got "triage"`},
		{url, []string{"--env", "prod", "--read-write", "pull", "--read-only", "admin"}, `postgres.access.readOnly: admin needs more permission than postgres.access.readWrite: pull; whoever may write may also read, so readOnly must need no more permission than readWrite`},
		{noPostgres, []string{"--env", "prod", "--read-only", "pull"}, `postgres.access.readOnly: pull needs postgres.enabled: true; there is no database to give access to`},
	} {
		head := headSubject(t, tc.url)
		_, stderr, code := dbAccess(t, tc.url, cli.Dependencies{}, append([]string{"shop"}, tc.args...)...)
		if code == 0 {
			t.Errorf("%v: exit 0, want a refusal", tc.args)
			continue
		}
		if !strings.Contains(stderr, tc.message) {
			t.Errorf("%v: stderr = %q, want %q", tc.args, stderr, tc.message)
		}
		if got := headSubject(t, tc.url); got != head {
			t.Errorf("%v: committed %q", tc.args, got)
		}
	}
}

func TestDBAccessRefusesAnEnvironmentWithoutADatabase(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML+"agePublicKey: "+testAgePublicKey+"\n")
	createShopApplication(t, url)
	if _, stderr, code := createApplication(t, url, cli.Dependencies{}, "--name", "tool", "--kind", "web-service", "--staging"); code != 0 {
		t.Fatalf("seeding tool: %s", stderr)
	}
	// staging's default read-write, push, is no refusal of its own.
	for _, args := range [][]string{{"shop", "--env", "prod"}, {"tool", "--env", "staging"}} {
		_, stderr, code := dbAccess(t, url, cli.Dependencies{}, args...)
		if code == 0 || !strings.Contains(stderr, "iidp app add-capability "+args[0]+" --postgres") {
			t.Errorf("%v: exit %d, stderr %q: want a refusal pointing at add-capability --postgres", args, code, stderr)
		}
	}
}

func TestDBAccessTakesProdOrStagingOnly(t *testing.T) {
	url := seedExistingDatabase(t)
	for _, env := range []string{"pr-3", "dev"} {
		_, stderr, code := dbAccess(t, url, cli.Dependencies{}, "shop", "--env", env)
		if code == 0 || !strings.Contains(stderr, "must be prod or staging") {
			t.Errorf("--env %s: exit %d, stderr %q; want it refused", env, code, stderr)
		}
	}
	_, stderr, code := dbAccess(t, url, cli.Dependencies{}, "shop")
	if code == 0 || !strings.Contains(stderr, `"env"`) {
		t.Errorf("no --env: exit %d, stderr %q; want --env required", code, stderr)
	}
}

func TestDBAccessNeedsTheAgeKeyToWriteAPassword(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	if _, stderr, code := createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service", "--postgres", "--staging"); code != 0 {
		t.Fatalf("seeding: %s", stderr)
	}
	_, stderr, code := dbAccess(t, url, cli.Dependencies{}, "shop", "--env", "staging")
	if code == 0 || !strings.Contains(stderr, "agePublicKey") {
		t.Errorf("exit %d, stderr %q: want a refusal naming agePublicKey", code, stderr)
	}
}

func TestDBAccessPasswordDecryptsWithTheRealSops(t *testing.T) {
	requireSops(t)
	url := seedExistingDatabase(t)
	if _, stderr, code := dbAccess(t, url, cli.Dependencies{Encryptor: sops.Binary{}}, "shop", "--env", "staging"); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	clone := cloneMain(t, url)
	doc := decryptSOPSFile(t, filepath.Join(clone, "applications", "shop", "staging", "sops", "db-write.enc.yaml"))
	data := doc["stringData"].(map[string]any)
	if doc["type"] != "kubernetes.io/basic-auth" || data["username"] != "shop_write" {
		t.Fatalf("decrypted = %v", doc)
	}
	assertPlaintextAbsent(t, clone, data["password"].(string))
}

// A secret stored as db-write.enc.yaml or db-read.enc.yaml would replace a
// role's password Secret.
func TestSecretSetRefusesTheKeysOfTheDatabasePasswords(t *testing.T) {
	url := seedExistingDatabase(t)
	for _, key := range []string{"DB_WRITE", "db_read"} {
		_, stderr, code := setSecret(t, url, cli.Dependencies{}, "", "shop", "staging", key+"=x")
		if code == 0 || !strings.Contains(stderr, "iidp app db access") {
			t.Errorf("%s: exit %d, stderr %q; want it refused", key, code, stderr)
		}
	}
}

// renderedRoles renders an Environment's committed values.yaml through the
// chart with an image tag, as its first deploy would, and returns the open
// managed roles of its Cluster by name. It skips without helm.
func renderedRoles(t *testing.T, clone, environment, cluster string) map[string]any {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not on PATH; skipping")
	}
	values := filepath.Join(clone, "applications", "shop", environment, "values.yaml")
	out, err := exec.Command("helm", "template", "shop", "../../chart/application", "--values", values, "--set", "image.tag=1.0.0").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			break
		}
		if obj["kind"] != "Cluster" || lookup(t, obj, "metadata", "name") != cluster {
			continue
		}
		roles := map[string]any{}
		if managed, ok := obj["spec"].(map[string]any)["managed"].(map[string]any); ok {
			for _, r := range managed["roles"].([]any) {
				if role := r.(map[string]any); role["ensure"] == "present" {
					roles[role["name"].(string)] = role
				}
			}
		}
		return roles
	}
	t.Fatalf("no Cluster %s rendered:\n%s", cluster, out)
	return nil
}

func TestAppCreatePostgresOpensEveryEnvironmentAtItsDefaults(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML+"agePublicKey: "+testAgePublicKey+"\n")

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{Encryptor: fakeEncryptor{}},
		"--name", "shop", "--kind", "web-service", "--postgres", "--staging")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	for _, want := range []string{"prod database:     read-write none · read-only none", "staging database:  read-write push · read-only none"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	clone := cloneMain(t, url)
	staging := postgresValues(t, clone, "staging")
	if access := staging["access"].(map[string]any); access["readWrite"] != "push" || access["readOnly"] != "none" {
		t.Errorf("staging postgres.access = %v", access)
	}
	if staging["readWritePasswordSecret"] != "shop-staging-db-write" {
		t.Errorf("staging readWritePasswordSecret = %v", staging["readWritePasswordSecret"])
	}
	assertPasswordSecret(t, filepath.Join(clone, "applications", "shop", "staging", "sops", "db-write.enc.yaml"), "shop-staging-db-write", "staging", "shop_write")
	if access := postgresValues(t, clone, "prod")["access"].(map[string]any); access["readWrite"] != "none" || access["readOnly"] != "none" {
		t.Errorf("prod postgres.access = %v", access)
	}
	if files := ksopsFiles(t, clone, "prod"); !slices.Equal(files, []string{"backups-credentials.enc.yaml"}) {
		t.Errorf("prod ksops.yaml files = %v, want no password", files)
	}
	if got := gitRun(t, clone, "log", "--format=%s"); strings.Count(got, "\n") != 2 {
		t.Errorf("history = %q, want the seed and one iidp app create commit", got)
	}

	roles := renderedRoles(t, clone, "staging", "shop-staging-db")
	if _, ok := roles["shop_write"]; !ok || len(roles) != 1 {
		t.Errorf("the new staging renders roles %v, want shop_write", roles)
	}
	if roles := renderedRoles(t, clone, "prod", "shop-db"); len(roles) != 0 {
		t.Errorf("the new prod renders roles %v, want none", roles)
	}
}

func TestAppCreatePostgresWithoutTheAgeKeyLeavesTheDatabaseClosed(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{Encryptor: fakeEncryptor{}},
		"--name", "shop", "--kind", "web-service", "--postgres", "--staging")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "iidp app db access shop --env staging") {
		t.Errorf("stdout does not say how to write the password later:\n%s", stdout)
	}
	clone := cloneMain(t, url)
	if _, named := postgresValues(t, clone, "staging")["readWritePasswordSecret"]; named {
		t.Error("staging names a password that was never written")
	}
	if roles := renderedRoles(t, clone, "staging", "shop-staging-db"); len(roles) != 0 {
		t.Errorf("staging renders roles %v, want none until its password is written", roles)
	}
}

func TestAppAddCapabilityPostgresOpensEveryEnvironmentAtItsDefaults(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML+"agePublicKey: "+testAgePublicKey+"\n")
	seedApplication(t, url, "--staging")

	stdout, stderr, code := addCapability(t, url, "shop", cli.Dependencies{Encryptor: fakeEncryptor{}}, "--postgres")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "staging database:  read-write push · read-only none") {
		t.Errorf("stdout does not print staging's levels:\n%s", stdout)
	}
	clone := cloneMain(t, url)
	assertPasswordSecret(t, filepath.Join(clone, "applications", "shop", "staging", "sops", "db-write.enc.yaml"), "shop-staging-db-write", "staging", "shop_write")
	if _, ok := renderedRoles(t, clone, "staging", "shop-staging-db")["shop_write"]; !ok {
		t.Error("staging does not render shop_write")
	}
	if access := postgresValues(t, clone, "prod")["access"].(map[string]any); access["readWrite"] != "none" {
		t.Errorf("prod postgres.access = %v, want closed", access)
	}
}

func TestAppAddCapabilityStagingOpensTheNewStagingsDatabase(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML+"agePublicKey: "+testAgePublicKey+"\n")
	seedApplication(t, url, "--postgres")
	if _, stderr, code := dbAccess(t, url, cli.Dependencies{}, "shop", "--env", "prod", "--read-only", "admin"); code != 0 {
		t.Fatalf("opening prod: %s", stderr)
	}

	if _, stderr, code := addCapability(t, url, "shop", cli.Dependencies{Encryptor: fakeEncryptor{}}, "--staging"); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	clone := cloneMain(t, url)
	staging := postgresValues(t, clone, "staging")
	if access := staging["access"].(map[string]any); access["readWrite"] != "push" || access["readOnly"] != "none" {
		t.Errorf("staging postgres.access = %v, want staging's defaults, not prod's", access)
	}
	if staging["readWritePasswordSecret"] != "shop-staging-db-write" || staging["readOnlyPasswordSecret"] != nil {
		t.Errorf("staging names %v and %v, want only its own read-write password", staging["readWritePasswordSecret"], staging["readOnlyPasswordSecret"])
	}
	assertPasswordSecret(t, filepath.Join(clone, "applications", "shop", "staging", "sops", "db-write.enc.yaml"), "shop-staging-db-write", "staging", "shop_write")
}

// Without sops a Create would make the Application repository and only then
// fail to write the Platform repository, so it is refused first.
func TestAppCreatePostgresRefusesWithoutSopsBeforeCreatingTheRepository(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(gitPath, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML+"agePublicKey: "+testAgePublicKey+"\n")
	gh := newFakeGitHub(t)
	t.Setenv("PATH", bin)

	_, stderr, code := createApplication(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs", "--postgres", "--staging")
	if code == 0 || !strings.Contains(stderr, "sops") {
		t.Fatalf("exit %d, stderr %q; want a refusal naming sops", code, stderr)
	}
	if n := gh.requestsTo(http.MethodPost, "/orgs/"+platform.Org+"/repos"); n != 0 {
		t.Errorf("created the Application repository %d times before refusing", n)
	}
}
