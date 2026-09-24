package deploygate_test

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/deploygate"
)

// A deploy may carry the migration command from the Application
// repository's iidp.yaml, which the gate writes with the tag
// (docs/implementation-notes/66-migration-command-in-repo.md). These tests
// use the same HTTP boundary, fakes and bare Platform repository as
// gate_test.go.

// addPostgresApplication seeds an Application like addApplication, with
// the postgres block app create writes: enabled as given, and command as
// its migration command in every Environment.
func addPostgresApplication(t *testing.T, url, name string, staging, enabled bool, command string) {
	t.Helper()
	dir := cloneMain(t, url)
	envs := []string{"prod"}
	if staging {
		envs = append(envs, "staging")
	}
	for _, environment := range envs {
		base := filepath.Join(dir, "applications", name, environment)
		writeFile(t, filepath.Join(base, "application.yaml"), "apiVersion: argoproj.io/v1alpha1\nkind: Application\n")
		writeFile(t, filepath.Join(base, "values.yaml"), fmt.Sprintf("# %s's %s values\napplication:\n  name: %s\nenvironment: %s\nimage:\n  repository: ghcr.io/itema-as/%s\n  tag: \"\"\nsize: small\npostgres:\n  enabled: %t\n  migrationCommand: %q\n  backupRetention: 30d\n", name, environment, name, environment, name, enabled, command))
	}
	writeFile(t, filepath.Join(dir, "applications", name, "repository.yaml"), binding(shopRepoID, orgID))
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "--amend", "--no-edit", "-m", "Seed the Platform repository")
	gitRun(t, dir, "push", "--force", "origin", "HEAD:main")
}

// deployWithMigration calls with a token carrying claims and the given
// migration command (nil: none sent).
func (e *env) deployWithMigration(claims map[string]any, environment, tag string, command *string) (int, map[string]any) {
	e.t.Helper()
	return e.call(e.issuer.sign(e.t, claims), deploygate.Request{Application: "shop", Environment: environment, Tag: tag, MigrationCommand: command})
}

// storedMigrationCommand is postgres.migrationCommand of an Environment on
// main.
func storedMigrationCommand(t *testing.T, url, environment string) any {
	t.Helper()
	var values struct {
		Postgres map[string]any `yaml:"postgres"`
	}
	data := readFile(t, filepath.Join(cloneMain(t, url), "applications/shop", environment, "values.yaml"))
	if err := yaml.Unmarshal([]byte(data), &values); err != nil {
		t.Fatal(err)
	}
	return values.Postgres["migrationCommand"]
}

func ptr(s string) *string { return &s }

func TestTheGateWritesTheMigrationCommandWithTheTagInOneCommit(t *testing.T) {
	e := newEnv(t)
	addPostgresApplication(t, e.platform, "shop", false, true, "")

	command := `npx prisma migrate deploy && echo "migrated: yes"`
	status, body := e.deployWithMigration(e.issuer.claims(), "auto", shaDeployed, &command)
	if status != http.StatusOK || body["migrationCommandChanged"] != true {
		t.Fatalf("status = %d, body = %v, want 200 and the migration command changed", status, body)
	}

	clone := cloneMain(t, e.platform)
	if got := gitRun(t, clone, "rev-list", "--count", "HEAD"); strings.TrimSpace(got) != "2" {
		t.Errorf("commits = %s, want the seed and one deploy", got)
	}
	if got := strings.TrimSpace(gitRun(t, clone, "show", "--name-only", "--format=", "HEAD")); got != "applications/shop/prod/values.yaml" {
		t.Errorf("the deploy changed %q, want only prod's values.yaml", got)
	}
	if got := gitRun(t, clone, "log", "-1", "--format=%B"); !strings.HasPrefix(got, "Deploy shop prod "+shaDeployed+"\n\nMigration command, from iidp.yaml: "+command+"\n") {
		t.Errorf("message = %q, want the subject and the new command", got)
	}
	values := readFile(t, filepath.Join(clone, "applications/shop/prod/values.yaml"))
	if !strings.Contains(values, "tag: "+shaDeployed) || !strings.Contains(values, "# shop's prod values") || !strings.Contains(values, "backupRetention: 30d") {
		t.Errorf("values.yaml lost the tag or the rest of the file:\n%s", values)
	}
	if got := storedMigrationCommand(t, e.platform, "prod"); got != command {
		t.Errorf("migrationCommand = %v, want %q", got, command)
	}

	// The same tag with the same command commits nothing; the same tag
	// with a new command (a re-run after editing iidp.yaml) commits it.
	if status, body := e.deployWithMigration(e.issuer.claims(), "auto", shaDeployed, &command); status != http.StatusOK || body["unchanged"] != true {
		t.Errorf("a repeat = %d %v, want 200 and unchanged", status, body)
	}
	if status, body := e.deployWithMigration(e.issuer.claims(), "auto", shaDeployed, ptr("npm run migrate")); status != http.StatusOK || body["unchanged"] == true || body["migrationCommandChanged"] != true {
		t.Errorf("a new command on the same tag = %d %v, want a commit", status, body)
	}
	if got := storedMigrationCommand(t, e.platform, "prod"); got != "npm run migrate" {
		t.Errorf("migrationCommand = %v, want npm run migrate", got)
	}
}

func TestTheGateLeavesTheMigrationCommandAloneWhenNoneIsSent(t *testing.T) {
	e := newEnv(t)
	addPostgresApplication(t, e.platform, "shop", false, true, "npx prisma migrate deploy")

	status, body := e.deployWithMigration(e.issuer.claims(), "auto", "sha1", nil)
	if status != http.StatusOK || body["migrationCommandChanged"] == true {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	if got := storedMigrationCommand(t, e.platform, "prod"); got != "npx prisma migrate deploy" {
		t.Errorf("migrationCommand = %v, want it left as it was", got)
	}
}

func TestTheGateClearsTheMigrationCommandOnAnExplicitEmptyOne(t *testing.T) {
	e := newEnv(t)
	addPostgresApplication(t, e.platform, "shop", false, true, "npx prisma migrate deploy")

	status, body := e.deployWithMigration(e.issuer.claims(), "auto", "sha1", ptr(""))
	if status != http.StatusOK || body["migrationCommandChanged"] != true {
		t.Fatalf("status = %d, body = %v, want the command cleared", status, body)
	}
	if got := storedMigrationCommand(t, e.platform, "prod"); got != "" {
		t.Errorf("migrationCommand = %#v, want \"\"", got)
	}
	if got := gitRun(t, cloneMain(t, e.platform), "log", "-1", "--format=%b"); !strings.Contains(got, "Clear the migration command") {
		t.Errorf("body = %q, want it to say the command was cleared", got)
	}
}

// The command reaches only the Environment the caller may deploy: main
// sets staging's, and prod keeps its own until a v* tag promotes.
func TestTheGateSetsTheMigrationCommandOnlyForTheEnvironmentDeployed(t *testing.T) {
	e := newEnv(t)
	addPostgresApplication(t, e.platform, "shop", true, true, "old-migrate")

	if status, body := e.deployWithMigration(e.issuer.claims(), "auto", "sha1", ptr("new-migrate")); status != http.StatusOK || body["environment"] != "staging" {
		t.Fatalf("status = %d, body = %v, want staging written", status, body)
	}
	if got := storedMigrationCommand(t, e.platform, "staging"); got != "new-migrate" {
		t.Errorf("staging's migrationCommand = %v, want new-migrate", got)
	}
	if got := storedMigrationCommand(t, e.platform, "prod"); got != "old-migrate" {
		t.Errorf("prod's migrationCommand = %v, want it untouched by a main deploy", got)
	}

	claims := e.issuer.claims()
	claims["ref"], claims["ref_type"] = "refs/tags/v1.0.0", "tag"
	if status, body := e.deployWithMigration(claims, "prod", "1.0.0", ptr("new-migrate")); status != http.StatusOK || body["environment"] != "prod" {
		t.Fatalf("promote: status = %d, body = %v", status, body)
	}
	if got := storedMigrationCommand(t, e.platform, "prod"); got != "new-migrate" {
		t.Errorf("prod's migrationCommand = %v, want the promoted commit's", got)
	}
}

func TestTheGateRefusesAMigrationCommandWithoutPostgres(t *testing.T) {
	e := newEnv(t)
	addPostgresApplication(t, e.platform, "shop", false, false, "")

	status, body := e.deployWithMigration(e.issuer.claims(), "auto", "sha1", ptr("npm run migrate"))
	e.refused(status, body, http.StatusConflict, "shop's prod Environment has no database to migrate", "Add the Postgres Capability first", "iidp app add-capability shop --postgres")

	// An iidp.yaml without a command is fine there: nothing to clear.
	if status, body := e.deployWithMigration(e.issuer.claims(), "auto", "sha1", ptr("")); status != http.StatusOK || body["migrationCommandChanged"] == true {
		t.Errorf("an empty command without Postgres = %d %v, want the tag deployed and nothing else", status, body)
	}
}

func TestTheGateRefusesAMigrationCommandThatIsNotOneShortLine(t *testing.T) {
	e := newEnv(t)
	addPostgresApplication(t, e.platform, "shop", false, true, "")

	for name, tc := range map[string]struct {
		command string
		want    string
	}{
		"over the limit": {strings.Repeat("x", 1025), "1025 bytes, more than the 1024 allowed"},
		"two lines":      {"npm run migrate\nnpm run seed", "must be one line"},
		"a control char": {"npm run migrate\x1b[0m", "control character"},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := e.deployWithMigration(e.issuer.claims(), "auto", "sha1", &tc.command)
			e.refused(status, body, http.StatusBadRequest, tc.want)
		})
	}
	// At the limit is fine.
	if status, body := e.deployWithMigration(e.issuer.claims(), "auto", "sha1", ptr(strings.Repeat("x", 1024))); status != http.StatusOK {
		t.Errorf("a 1024-byte command = %d %v, want 200", status, body)
	}
}

// The migration command gets no further than the tag would: a caller the
// gate refuses changes nothing, whatever it sends.
func TestTheGateChecksTheCallerBeforeTheMigrationCommand(t *testing.T) {
	e := newEnv(t)
	addPostgresApplication(t, e.platform, "shop", false, true, "")
	claims := e.issuer.claims()
	claims["repository"], claims["repository_id"] = "Itema-as/other", "999"

	status, body := e.deployWithMigration(claims, "auto", "sha1", ptr("npm run migrate"))
	e.refused(status, body, http.StatusForbidden, "repository id 999")
}
