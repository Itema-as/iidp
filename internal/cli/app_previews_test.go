package cli_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/platform"
)

// Preview Environments (#95): --previews writes an ArgoCD ApplicationSet,
// applications/<name>/previews/applicationset.yaml, whose Pull Request
// generator gives every open pull request labelled preview an Environment
// rendered from staging's application.yaml
// (docs/adr/0006-preview-environments-from-an-argocd-applicationset.md).

const previewsFile = "applications/shop/previews/applicationset.yaml"

// previewSet is the part of the ApplicationSet the tests read.
type previewSet struct {
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		GoTemplate bool `yaml:"goTemplate"`
		Generators []struct {
			PullRequest struct {
				GitHub struct {
					Owner         string   `yaml:"owner"`
					Repo          string   `yaml:"repo"`
					API           string   `yaml:"api"`
					AppSecretName string   `yaml:"appSecretName"`
					Labels        []string `yaml:"labels"`
				} `yaml:"github"`
				RequeueAfterSeconds int `yaml:"requeueAfterSeconds"`
			} `yaml:"pullRequest"`
		} `yaml:"generators"`
		Template struct {
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Sources     []map[string]any `yaml:"sources"`
				Destination struct {
					Namespace string `yaml:"namespace"`
				} `yaml:"destination"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func readPreviewSet(t *testing.T, clone string) previewSet {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(clone, previewsFile))
	if err != nil {
		t.Fatalf("reading %s: %v", previewsFile, err)
	}
	var set previewSet
	if err := yaml.Unmarshal(data, &set); err != nil {
		t.Fatalf("parsing %s: %v\n%s", previewsFile, err, data)
	}
	if len(set.Spec.Generators) != 1 {
		t.Fatalf("%s has %d generators, want one", previewsFile, len(set.Spec.Generators))
	}
	return set
}

// sourcePaths lists the path of every source that has one: the sops/
// directory is the only one the CLI writes.
func sourcePaths(sources []map[string]any) []string {
	var paths []string
	for _, s := range sources {
		if p, ok := s["path"].(string); ok {
			paths = append(paths, p)
		}
	}
	return paths
}

// createWithPreviews makes shop through the Create path with staging,
// previews and the extra flags.
func createWithPreviews(t *testing.T, url string, gh *fakeGitHub, extra ...string) (stdout string) {
	t.Helper()
	args := append([]string{"--name", "shop", "--path", "create", "--framework", "nextjs", "--staging", "--previews"}, extra...)
	stdout, stderr, code := createApplication(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL}, args...)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	return stdout
}

func TestAppCreatePreviewsWritesTheApplicationSet(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)

	stdout := createWithPreviews(t, url, gh, "--postgres", "--login", "--login-group", groupA)

	clone := cloneMain(t, url)
	set := readPreviewSet(t, clone)
	if set.Metadata.Name != "shop-previews" || set.Metadata.Namespace != "argocd" || !set.Spec.GoTemplate {
		t.Errorf("ApplicationSet %s/%s goTemplate %v, want argocd/shop-previews with Go templates", set.Metadata.Namespace, set.Metadata.Name, set.Spec.GoTemplate)
	}
	pr := set.Spec.Generators[0].PullRequest
	if pr.GitHub.Owner != platform.Org || pr.GitHub.Repo != "shop" {
		t.Errorf("the generator follows %s/%s, want the bound repository %s/shop", pr.GitHub.Owner, pr.GitHub.Repo, platform.Org)
	}
	if !slices.Equal(pr.GitHub.Labels, []string{"preview"}) || pr.RequeueAfterSeconds != 180 {
		t.Errorf("labels %v, requeue %d, want [preview] every 180 seconds", pr.GitHub.Labels, pr.RequeueAfterSeconds)
	}
	if pr.GitHub.AppSecretName != "platform-repo-github-app" || pr.GitHub.API != "" {
		t.Errorf("appSecretName %q, api %q, want the App credential cloud-init writes and GitHub.com", pr.GitHub.AppSecretName, pr.GitHub.API)
	}
	if set.Spec.Template.Metadata.Name != "shop-pr-{{.number}}" || set.Spec.Template.Spec.Destination.Namespace != "shop-pr-{{.number}}" {
		t.Errorf("template %s in %s, want shop-pr-{{.number}} in its own namespace", set.Spec.Template.Metadata.Name, set.Spec.Template.Spec.Destination.Namespace)
	}
	// Staging's sources, sops/ included, since --postgres gave staging its
	// backups-credentials Secret there.
	if got := sourcePaths(set.Spec.Template.Spec.Sources); !slices.Equal(got, []string{"applications/shop/staging/sops"}) {
		t.Errorf("the template's kustomize sources are %v, want staging's sops/", got)
	}

	// In the same commit as the Environments.
	touched := strings.Fields(gitRun(t, clone, "show", "--name-only", "--format=", "HEAD"))
	if !slices.Contains(touched, previewsFile) || !slices.Contains(touched, "applications/shop/staging/application.yaml") {
		t.Errorf("iidp app create committed %v, want the ApplicationSet with the Environments", touched)
	}
	for _, want := range []string{"previews: https://shop-pr-<number>.app.itma.no, for each open pull request labelled preview", previewsFile} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

// A Platform whose pull requests are on another GitHub (Enterprise, or the
// kind e2e's fake) names its API in platform.yaml.
func TestAppCreatePreviewsNameTheGitHubAPIFromPlatformYAML(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML+"githubAPI: https://github.example.com/api/v3\n")
	gh := newFakeGitHub(t)

	createWithPreviews(t, url, gh)

	if got := readPreviewSet(t, cloneMain(t, url)).Spec.Generators[0].PullRequest.GitHub.API; got != "https://github.example.com/api/v3" {
		t.Errorf("api = %q, want platform.yaml's githubAPI", got)
	}
}

func TestAppCreatePreviewsRefusedWithoutStaging(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)

	_, stderr, code := createApplication(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs", "--previews")

	if code == 0 {
		t.Fatal("exit code = 0, want a refusal")
	}
	if !strings.Contains(stderr, "previews use staging's secrets; add --staging first") {
		t.Errorf("stderr = %q, want it to ask for --staging first", stderr)
	}
	if n := gh.requestsTo("POST", "/orgs/"+platform.Org+"/repos"); n != 0 {
		t.Errorf("the Application repository was created (%d requests) before the refusal", n)
	}
	assertNoApplications(t, url)
}

// Without --path there is no Application repository, so no binding and no
// pull requests to follow.
func TestAppCreatePreviewsRefusedWithoutABinding(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	_, stderr, code := createApplication(t, url, cli.Dependencies{}, "--name", "shop", "--kind", "web-service", "--staging", "--previews")

	if code == 0 {
		t.Fatal("exit code = 0, want a refusal")
	}
	for _, want := range []string{"bound to none", "--path create or --path adopt"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to say %q", stderr, want)
		}
	}
	assertNoApplications(t, url)
}

func TestAppCreateWithoutPreviewsWritesNoApplicationSet(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)

	_, stderr, code := createApplication(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs", "--staging")

	if code != 0 {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(cloneMain(t, url), previewsFile)); err == nil {
		t.Errorf("%s exists without --previews", previewsFile)
	}
}

// The wizard asks right after the staging question, and only when staging
// is on.
func TestAppCreateWizardAsksForPreviewsOnlyWithStaging(t *testing.T) {
	for _, tc := range []struct {
		name, stdin string
		staging     bool
	}{
		// staging, previews, domain, login, size, confirm.
		{"with staging", "y\ny\n\n\n\ny\n", true},
		// staging, domain, login, size, confirm.
		{"without staging", "n\n\n\n\ny\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
			gh := newFakeGitHub(t)

			stdout, stderr, code := createApplicationInteractive(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL}, tc.stdin,
				"--name", "shop", "--path", "create", "--framework", "nextjs", "--postgres=false")

			if code != 0 {
				t.Fatalf("exit code = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			asked := strings.Contains(stdout, "Preview Environments?")
			if asked != tc.staging {
				t.Errorf("asked about previews: %v, want %v\n%s", asked, tc.staging, stdout)
			}
			if tc.staging {
				if i, j := strings.Index(stdout, "Staging Environment?"), strings.Index(stdout, "Preview Environments?"); i < 0 || j < i {
					t.Errorf("the previews question does not follow the staging one:\n%s", stdout)
				}
				if !strings.Contains(stdout, "Previews:   enabled, for pull requests labelled preview (https://shop-pr-<number>.app.itma.no)") {
					t.Errorf("the summary does not show the previews:\n%s", stdout)
				}
			}
			_, err := os.Stat(filepath.Join(cloneMain(t, url), previewsFile))
			if written := err == nil; written != tc.staging {
				t.Errorf("%s written: %v, want %v", previewsFile, written, tc.staging)
			}
		})
	}
}

func TestAppAddCapabilityPreviewsWritesTheApplicationSet(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)
	_, stderr, code := createApplication(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs", "--staging")
	if code != 0 {
		t.Fatalf("creating shop: %d\n%s", code, stderr)
	}

	stdout, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--previews")

	if code != 0 {
		t.Fatalf("exit code = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	clone := cloneMain(t, url)
	if got := readPreviewSet(t, clone).Spec.Generators[0].PullRequest.GitHub.Repo; got != "shop" {
		t.Errorf("the generator follows %q, want shop", got)
	}
	if got := headSubject(t, url); got != "iidp app add-capability shop previews" {
		t.Errorf("head commit = %q", got)
	}
	if touched := strings.Fields(gitRun(t, clone, "show", "--name-only", "--format=", "HEAD")); !slices.Equal(touched, []string{previewsFile}) {
		t.Errorf("the commit touched %v, want only %s", touched, previewsFile)
	}
	if !strings.Contains(stdout, "previews: https://shop-pr-<number>.app.itma.no") {
		t.Errorf("stdout lacks the previews' address:\n%s", stdout)
	}

	// A second time is refused, as for every Capability already present.
	_, stderr, code = addCapability(t, url, "shop", cli.Dependencies{}, "--previews")
	if code == 0 || !strings.Contains(stderr, "already has Preview Environments") {
		t.Errorf("a second --previews: exit %d, stderr %q, want a refusal saying they are there", code, stderr)
	}
}

// --staging and --previews together: the new staging Environment is what
// the previews render from.
func TestAppAddCapabilityStagingAndPreviewsTogether(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)
	createBound(t, url, gh)

	_, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--staging", "--previews")

	if code != 0 {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	readPreviewSet(t, clone)
	if got := headSubject(t, url); got != "iidp app add-capability shop staging previews" {
		t.Errorf("head commit = %q", got)
	}
}

func TestAppAddCapabilityPreviewsRefusedWithoutStagingOrBinding(t *testing.T) {
	gh := newFakeGitHub(t)

	unstaged := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	createBound(t, unstaged, gh)
	unbound := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, unbound, "--staging")

	for _, tc := range []struct {
		name, url string
		want      []string
	}{
		{"without staging", unstaged, []string{"previews use staging's secrets; add --staging first"}},
		{"without a binding", unbound, []string{"bound to none", "iidp app bind shop --repo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := headSubject(t, tc.url)
			_, stderr, code := addCapability(t, tc.url, "shop", cli.Dependencies{}, "--previews")
			if code == 0 {
				t.Fatal("exit code = 0, want a refusal")
			}
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr = %q, want it to say %q", stderr, want)
				}
			}
			if after := headSubject(t, tc.url); after != before {
				t.Errorf("committed %q despite the refusal", after)
			}
		})
	}
}

// Postgres added later gives staging its sops/ source, and the previews get
// it in the same commit, so they keep getting staging's secrets.
func TestAppAddCapabilityPostgresRefreshesThePreviews(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)
	createWithPreviews(t, url, gh)
	if got := sourcePaths(readPreviewSet(t, cloneMain(t, url)).Spec.Template.Spec.Sources); len(got) != 0 {
		t.Fatalf("before Postgres the template has kustomize sources %v, want none", got)
	}

	_, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--postgres")

	if code != 0 {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	if got := sourcePaths(readPreviewSet(t, clone).Spec.Template.Spec.Sources); !slices.Equal(got, []string{"applications/shop/staging/sops"}) {
		t.Errorf("after Postgres the template has kustomize sources %v, want staging's sops/", got)
	}
	if touched := strings.Fields(gitRun(t, clone, "show", "--name-only", "--format=", "HEAD")); !slices.Contains(touched, previewsFile) {
		t.Errorf("the commit touched %v, want the ApplicationSet too", touched)
	}

	// A Capability that leaves staging's application.yaml alone leaves the
	// ApplicationSet alone.
	_, stderr, code = addCapability(t, url, "shop", cli.Dependencies{}, "--size", "medium")
	if code != 0 {
		t.Fatalf("--size: exit code = %d\nstderr: %s", code, stderr)
	}
	if touched := strings.Fields(gitRun(t, cloneMain(t, url), "show", "--name-only", "--format=", "HEAD")); slices.Contains(touched, previewsFile) {
		t.Errorf("--size touched %v, want the ApplicationSet unchanged", touched)
	}
}

// staging's first secret adds its sops/ source; the previews follow.
func TestSecretSetOnStagingRefreshesThePreviews(t *testing.T) {
	requireSops(t)
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML+"agePublicKey: "+testAgePublicKey+"\n")
	gh := newFakeGitHub(t)
	createWithPreviews(t, url, gh)

	_, stderr, code := setSecret(t, url, cli.Dependencies{}, "", "shop", "staging", "API_KEY=s3cret")

	if code != 0 {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr)
	}
	if got := sourcePaths(readPreviewSet(t, cloneMain(t, url)).Spec.Template.Spec.Sources); !slices.Equal(got, []string{"applications/shop/staging/sops"}) {
		t.Errorf("the template's kustomize sources are %v, want staging's sops/", got)
	}
}

func TestAppDeleteRemovesThePreviews(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)
	createWithPreviews(t, url, gh)

	stdout, stderr, code := deleteApplication(t, url, "shop", "", cli.Dependencies{}, "--force")

	if code != 0 {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr)
	}
	clone := cloneMain(t, url)
	if _, err := os.Stat(filepath.Join(clone, previewsFile)); err == nil {
		t.Errorf("%s is still there after iidp app delete", previewsFile)
	}
	// Staging's values and secrets stay, as for every Environment: the
	// previews render from them until ArgoCD has deleted them.
	if _, err := os.Stat(filepath.Join(clone, "applications/shop/staging/values.yaml")); err != nil {
		t.Errorf("staging's values.yaml is gone: %v", err)
	}
	if !strings.Contains(stdout, "Removed the Preview Environments too") {
		t.Errorf("stdout does not mention the previews:\n%s", stdout)
	}
}
