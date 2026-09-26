package cli_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/cli"
)

// Sign-in groups (#92): --login-group restricts Itema login to the members
// of Entra groups, written as login.groups into every Environment. The CLI
// checks only the shape of each id, a GUID, and lowercases it
// (docs/implementation-notes/92-sign-in-groups.md).

const (
	groupA = "0f3b6a4e-8c1d-4e2f-9a7b-5c6d7e8f9a0b"
	groupB = "6e1d2c3b-4a5f-4b6c-8d7e-9f0a1b2c3d4e"
)

// loginGroups reads login.groups from an Environment's values.yaml on the
// Platform repository's main.
func loginGroups(t *testing.T, clone, env string) []string {
	t.Helper()
	values := readYAML(t, filepath.Join(clone, "applications/shop", env, "values.yaml"))
	raw, ok := lookup(t, values, "login", "groups").([]any)
	if !ok {
		t.Fatalf("%s login.groups = %v, want a list", env, lookup(t, values, "login", "groups"))
	}
	groups := make([]string, len(raw))
	for i, g := range raw {
		groups[i] = g.(string)
	}
	return groups
}

func TestAppCreateLoginGroupsWrittenToEveryEnvironment(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--staging", "--login",
		"--login-group", groupA, "--login-group", strings.ToUpper(groupB))

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	for _, env := range []string{"prod", "staging"} {
		// In the order given, the upper-case one lowercased.
		if got := loginGroups(t, clone, env); strings.Join(got, ",") != groupA+","+groupB {
			t.Errorf("%s login.groups = %v, want [%s %s]", env, got, groupA, groupB)
		}
	}
	if want := "Sign-in groups: " + groupA + ", " + groupB; !strings.Contains(stdout, want) {
		t.Errorf("stdout lacks %q:\n%s", want, stdout)
	}
}

// Without --login-group every Environment still states its groups, none,
// the same way domains: [] is always written.
func TestAppCreateLoginWithoutGroupsWritesAnEmptyList(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--login")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if got := loginGroups(t, cloneMain(t, url), "prod"); len(got) != 0 {
		t.Errorf("login.groups = %v, want empty", got)
	}
	if want := "Sign-in groups: none, every Itema user gets in"; !strings.Contains(stdout, want) {
		t.Errorf("stdout lacks %q:\n%s", want, stdout)
	}
}

func TestAppCreateRefusesLoginGroups(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"without --login", []string{"--login-group", groupA}, "sign-in groups need Itema login: give --login with --login-group"},
		{"not a GUID", []string{"--login", "--login-group", "platform-team"}, `--login-group "platform-team" is not an Entra group object id`},
		{"a GUID with more after it", []string{"--login", "--login-group", groupA + "&allowed_emails=x"}, "is not an Entra group object id"},
		{"given twice", []string{"--login", "--login-group", groupA, "--login-group", strings.ToUpper(groupA)}, "--login-group " + groupA + " is given twice"},
		{"empty next to an id", []string{"--login", "--login-group", groupA, "--login-group", ""}, "--login-group '' removes every sign-in group and cannot be given together with group ids"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
			before := headSubject(t, url)

			_, stderr, code := createApplication(t, url, cli.Dependencies{},
				append([]string{"--name", "shop", "--kind", "web-service"}, tc.args...)...)

			if code == 0 {
				t.Fatalf("exit code = 0, want non-zero")
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want it to say %q", stderr, tc.want)
			}
			if got := headSubject(t, url); got != before {
				t.Errorf("Platform repository has a new commit %q, want none", got)
			}
		})
	}
}

// The not-a-GUID refusal says where a group's object id is found.
func TestAppCreateLoginGroupRefusalSaysWhereToFindTheID(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	_, stderr, _ := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--login", "--login-group", "Platform Team")

	if !strings.Contains(stderr, "az ad group show --group <name> --query id -o tsv") {
		t.Errorf("stderr = %q, want it to say how to find a group's object id", stderr)
	}
}

func TestAppAddCapabilityLoginGroupsReplaceTheListInEveryEnvironment(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--staging", "--login", "--login-group", groupA)

	stdout, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--login-group", groupB)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	for _, env := range []string{"prod", "staging"} {
		if got := loginGroups(t, clone, env); strings.Join(got, ",") != groupB {
			t.Errorf("%s login.groups = %v, want [%s], replaced rather than added to", env, got, groupB)
		}
	}
	if got := headSubject(t, url); got != "iidp app add-capability shop login-group" {
		t.Errorf("commit subject = %q, want %q", got, "iidp app add-capability shop login-group")
	}
	if want := "Sign-in groups: " + groupB; !strings.Contains(stdout, want) {
		t.Errorf("stdout lacks %q:\n%s", want, stdout)
	}
}

// An empty --login-group removes every group: any Itema user gets in
// again.
func TestAppAddCapabilityEmptyLoginGroupRemovesTheGroups(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--staging", "--login", "--login-group", groupA, "--login-group", groupB)

	stdout, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--login-group", "")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	for _, env := range []string{"prod", "staging"} {
		if got := loginGroups(t, clone, env); len(got) != 0 {
			t.Errorf("%s login.groups = %v, want empty", env, got)
		}
		values := readYAML(t, filepath.Join(clone, "applications/shop", env, "values.yaml"))
		if got := lookup(t, values, "login", "enabled"); got != true {
			t.Errorf("%s login.enabled = %v, want login left on", env, got)
		}
	}
	if want := "Sign-in groups: none, every Itema user gets in"; !strings.Contains(stdout, want) {
		t.Errorf("stdout lacks %q:\n%s", want, stdout)
	}
}

// --login with --login-group turns login on for those groups only, in one
// commit.
func TestAppAddCapabilityLoginWithLoginGroups(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--staging")

	_, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--login", "--login-group", groupA)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	for _, env := range []string{"prod", "staging"} {
		values := readYAML(t, filepath.Join(clone, "applications/shop", env, "values.yaml"))
		if got := lookup(t, values, "login", "enabled"); got != true {
			t.Errorf("%s login.enabled = %v, want true", env, got)
		}
		if got := loginGroups(t, clone, env); strings.Join(got, ",") != groupA {
			t.Errorf("%s login.groups = %v, want [%s]", env, got, groupA)
		}
	}
	if got := headSubject(t, url); got != "iidp app add-capability shop login login-group" {
		t.Errorf("commit subject = %q", got)
	}
}

// A staging Environment added later takes prod's groups with the rest of
// prod's values.
func TestAppAddCapabilityStagingKeepsTheLoginGroups(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--login", "--login-group", groupA)

	if _, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--staging"); code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if got := loginGroups(t, cloneMain(t, url), "staging"); strings.Join(got, ",") != groupA {
		t.Errorf("staging login.groups = %v, want [%s]", got, groupA)
	}
}

func TestAppAddCapabilityRefusesLoginGroups(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed []string
		args []string
		want string
	}{
		{"without login", nil, []string{"--login-group", groupA}, `sign-in groups need Itema login: "shop" does not have Itema login; give --login with --login-group`},
		{"removing them without login", nil, []string{"--login-group", ""}, "sign-in groups need Itema login"},
		{"not a GUID", []string{"--login"}, []string{"--login-group", "not-a-group"}, `--login-group "not-a-group" is not an Entra group object id`},
		{"the same groups", []string{"--login", "--login-group", groupA}, []string{"--login-group", strings.ToUpper(groupA)}, "the sign-in groups of \"shop\" are already " + groupA},
		{"removing none", []string{"--login"}, []string{"--login-group", ""}, `"shop" has no sign-in groups`},
		{"--login again", []string{"--login"}, []string{"--login", "--login-group", groupA}, "to change its sign-in groups, give --login-group without --login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
			seedApplication(t, url, tc.seed...)
			before := headSubject(t, url)

			_, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, tc.args...)

			if code == 0 {
				t.Fatalf("exit code = 0, want non-zero")
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want it to say %q", stderr, tc.want)
			}
			if got := headSubject(t, url); got != before {
				t.Errorf("Platform repository has a new commit %q, want none", got)
			}
		})
	}
}

// The wizard asks for groups right after the login question when login is
// on, says how to find a group's object id, asks again on a bad answer, and
// lists the groups in the summary.
func TestAppCreateWizardAsksForSignInGroupsAfterLogin(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)

	// postgres, staging, domain, login, groups (a bad answer, then two
	// ids), size, confirm.
	stdin := "n\nn\n\ny\nplatform-team\n" + groupA + ", " + strings.ToUpper(groupB) + "\n\ny\n"

	stdout, stderr, code := createApplicationInteractive(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL}, stdin,
		"--name", "shop", "--path", "create", "--framework", "nextjs")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	login := strings.Index(stdout, "Itema login?")
	groups := strings.Index(stdout, "Sign-in groups [none]:")
	size := strings.Index(stdout, "Size")
	if login < 0 || groups < login || size < groups {
		t.Errorf("want the sign-in groups question between the login and size questions:\n%s", stdout)
	}
	for _, want := range []string{
		"az ad group show --group <name> --query id -o tsv",
		`--login-group "platform-team" is not an Entra group object id`,
		"  Sign-in groups: " + groupA + ", " + groupB,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if got := loginGroups(t, cloneMain(t, url), "prod"); strings.Join(got, ",") != groupA+","+groupB {
		t.Errorf("login.groups = %v, want [%s %s]", got, groupA, groupB)
	}
}

// Without login the question is never asked.
func TestAppCreateWizardSkipsSignInGroupsWithoutLogin(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)

	// postgres, staging, domain, login, size, confirm.
	stdin := "n\nn\n\nn\n\ny\n"

	stdout, stderr, code := createApplicationInteractive(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL}, stdin,
		"--name", "shop", "--path", "create", "--framework", "nextjs")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if strings.Contains(stdout, "Sign-in groups") {
		t.Errorf("stdout mentions sign-in groups without login:\n%s", stdout)
	}
}
