package cli_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/platform"
)

// createApplicationInteractive runs iidp app create --interactive in-process,
// scripting the wizard's answers as stdin (one per line, in the order
// docs/design.md's wizard asks them), against the same bare Platform
// repository and fake GitHub server the flag-driven tests use. --interactive
// is the hidden flag that forces the wizard without a real terminal
// (docs/implementation-notes/14-cli-wizard.md).
func createApplicationInteractive(t *testing.T, url string, deps cli.Dependencies, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if deps.TokenSource == nil {
		deps.TokenSource = fakeTokenSource{token: "gho_test"}
	}
	full := append([]string{"app", "create", "--platform-repo", url, "--interactive"}, args...)
	var out, errOut strings.Builder
	code = cli.RunWith(full, strings.NewReader(stdin), &out, &errOut, deps)
	return out.String(), errOut.String(), code
}

func TestAppCreateWizardFullRunAnsweringEveryQuestion(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)

	// name, path, framework, postgres, migration command, staging, domain,
	// size, confirm.
	stdin := "shop\ncreate\nnextjs\ny\n\nn\n\n\ny\n"

	stdout, stderr, code := createApplicationInteractive(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL}, stdin)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	clone := cloneAppRepo(t, gh, platform.Org, "shop")
	assertFileExists(t, filepath.Join(clone, "Dockerfile"))

	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	for _, tc := range []struct {
		path []any
		want any
	}{
		{[]any{"kind"}, "web-service"},
		{[]any{"postgres", "enabled"}, true},
		{[]any{"postgres", "migrationCommand"}, ""},
		{[]any{"size"}, "small"},
	} {
		if got := lookup(t, values, tc.path...); got != tc.want {
			t.Errorf("values.yaml %v = %v, want %v", tc.path, got, tc.want)
		}
	}
	if got, ok := lookup(t, values, "domains").([]any); !ok || len(got) != 0 {
		t.Errorf("values.yaml domains = %v, want empty", lookup(t, values, "domains"))
	}

	for _, want := range []string{"Application name", "Create or Adopt", "Framework", "Postgres database", "Migration command", "Staging Environment", "Custom domain", "Size", "Proceed"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks the question %q:\n%s", want, stdout)
		}
	}

	for _, want := range []string{
		"Summary:", "Name:       shop", "Owner:      " + platform.Org, "Framework:  nextjs",
		"Visibility: private", "Kind:       web-service", "Size:       small", "Port:       3000",
		"Probe path: /", "Postgres:   enabled", "Staging:    disabled", "Domains:    none",
		"ArgoCD:     https://argocd.platform.itma.no", "Grafana:    https://itema.grafana.net",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("summary lacks %q:\n%s", want, stdout)
		}
	}
}

// TestAppCreateWizardFlagsPreAnswerQuestions checks that a question whose
// flag was already given is skipped entirely: it is never printed, and the
// stdin script does not need to answer it.
func TestAppCreateWizardFlagsPreAnswerQuestions(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)

	// Only postgres, staging, domain, size and the confirmation are asked;
	// name, path and framework were given as flags.
	stdin := "n\nn\n\n\ny\n"

	stdout, stderr, code := createApplicationInteractive(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL}, stdin,
		"--name", "shop", "--path", "create", "--framework", "nextjs")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, notWant := range []string{"Application name:", "Create or Adopt", "Framework:\n"} {
		if strings.Contains(stdout, notWant) {
			t.Errorf("stdout contains the pre-answered question %q, want it skipped:\n%s", notWant, stdout)
		}
	}
	for _, want := range []string{"Postgres database", "Staging Environment", "Custom domain", "Size"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks the still-open question %q:\n%s", want, stdout)
		}
	}
	if gh.cloneURL(platform.Org, "shop") == "" {
		t.Fatalf("the Application repository was not created")
	}
}

// TestAppCreateWizardDefaultsTakenByEmptyAnswers checks that a blank answer
// takes the bracketed default docs/design.md's wizard names for each
// question.
func TestAppCreateWizardDefaultsTakenByEmptyAnswers(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)

	stdin := "\n\n\n\n\n" // postgres, staging, domain, size, confirm: all blank

	stdout, stderr, code := createApplicationInteractive(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL}, stdin,
		"--name", "shop", "--path", "create", "--framework", "nextjs")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	for _, tc := range []struct {
		path []any
		want any
	}{
		{[]any{"postgres", "enabled"}, false},
		{[]any{"size"}, "small"},
	} {
		if got := lookup(t, values, tc.path...); got != tc.want {
			t.Errorf("values.yaml %v = %v, want %v (the default)", tc.path, got, tc.want)
		}
	}
	if got, ok := lookup(t, values, "domains").([]any); !ok || len(got) != 0 {
		t.Errorf("values.yaml domains = %v, want empty (the default)", lookup(t, values, "domains"))
	}
}

// TestAppCreateWizardInvalidNameIsReAsked checks that an invalid answer is
// rejected with an explanation and the question asked again, without ever
// reaching the GitHub API or the Platform repository.
func TestAppCreateWizardInvalidNameIsReAsked(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)

	stdin := "Not_Valid!\nshop\ncreate\nnextjs\nn\nn\n\n\ny\n"

	stdout, stderr, code := createApplicationInteractive(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL}, stdin)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Not_Valid!") || !strings.Contains(stdout, "lowercase") {
		t.Errorf("stdout does not explain the invalid name and re-ask:\n%s", stdout)
	}
	if n := strings.Count(stdout, "Application name: "); n < 2 {
		t.Errorf("the name prompt was printed %d times, want at least 2 (asked, then re-asked):\n%s", n, stdout)
	}
	if gh.cloneURL(platform.Org, "shop") == "" {
		t.Fatalf("the Application repository was not created after the valid name")
	}
}

// TestAppCreateWizardMigrationQuestionShowsHelpTextAndDetectedSuggestion
// checks docs/design.md's exact migration help text and that a detected
// migration tool's suggested command is offered as the default.
func TestAppCreateWizardMigrationQuestionShowsHelpTextAndDetectedSuggestion(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "prisma", "schema.prisma"), "// schema\n")

	// postgres=y, migration command blank (accept the suggestion), staging=n,
	// domain blank, size blank, confirm=y.
	stdin := "y\n\nn\n\n\ny\n"

	stdout, stderr, code := createApplicationInteractive(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL}, stdin,
		"--name", "shop", "--path", "create", "--framework", "nextjs", "--app-dir", dir)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, want := range []string{
		"Migration command (optional)",
		"Runs once before every rollout, in a one-off container built from your",
		"image, with DATABASE_URL set. Leave empty if your app has no migrations.",
		"Detected: Prisma", "npx prisma migrate deploy",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "postgres", "migrationCommand"); got != "npx prisma migrate deploy" {
		t.Errorf("values.yaml postgres.migrationCommand = %v, want the accepted suggestion", got)
	}
}

// TestAppCreateWizardDecliningTheSummaryHasNoSideEffects checks the
// acceptance criterion by name: declining exits 0, and nothing was cloned
// from GitHub or pushed to the Platform repository.
func TestAppCreateWizardDecliningTheSummaryHasNoSideEffects(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)

	// Only the migration command, domain, size and confirmation are asked
	// (name, path, framework, postgres and staging were given as flags).
	stdin := "\n\n\nn\n"

	stdout, stderr, code := createApplicationInteractive(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL}, stdin,
		"--name", "shop", "--path", "create", "--framework", "nextjs",
		"--postgres", "--staging")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (declining is not a failure)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if n := gh.requestsTo(http.MethodPost, "/repos"); n != 0 {
		t.Errorf("a repository creation request was made despite declining")
	}
	if n := gh.requestsTo(http.MethodPost, "/orgs/"+platform.Org+"/repos"); n != 0 {
		t.Errorf("POST /orgs/%s/repos called %d times, want 0", platform.Org, n)
	}
	assertNoApplications(t, url)
}

// TestAppCreateWizardAdoptIsNotImplementedYet checks the wizard's hook for
// the Adopt path (#15): choosing it stops asking further questions and
// fails with the pre-existing "not implemented" message, with no GitHub
// request made and nothing written to the Platform repository.
func TestAppCreateWizardAdoptIsNotImplementedYet(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)

	stdin := "shop\nadopt\n"

	stdout, stderr, code := createApplicationInteractive(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL}, stdin)

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr, "not implemented") || !strings.Contains(stderr, "15") {
		t.Errorf("stderr = %q, want it to say Adopt is not implemented and name issue #15", stderr)
	}
	if !strings.Contains(stdout, "not available yet") {
		t.Errorf("stdout = %q, want the wizard's own notice", stdout)
	}
	if len(gh.requests) != 0 {
		t.Errorf("the fake GitHub API was called %d times, want 0", len(gh.requests))
	}
	assertNoApplications(t, url)
}

func TestAppCreateWizardWithoutTTYAndWithoutInteractiveFlagBehavesAsBefore(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)

	full := append([]string{"app", "create", "--platform-repo", url}, "--name", "shop")
	var out, errOut strings.Builder
	code := cli.RunWith(full, strings.NewReader(""), &out, &errOut, cli.Dependencies{TokenSource: fakeTokenSource{token: "gho_test"}})

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero: no --kind and no wizard to ask for one")
	}
	if !strings.Contains(errOut.String(), `"kind" not set`) {
		t.Errorf("stderr = %q, want the pre-existing required-flag message", errOut.String())
	}
	assertNoApplications(t, url)
}
