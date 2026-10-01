// Package platform_test checks infra/platform's cloud-init template. Most
// tests read it as text rather than reimplementing templatefile(); `tofu
// validate` and TestOpenTofuRendersRegistriesYAML cover a real render.
package platform_test

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const cloudInitTemplate = "cloud-init/user-data.yaml.tftpl"

func readCloudInitTemplate(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(cloudInitTemplate)
	if err != nil {
		t.Fatalf("reading %s: %v", cloudInitTemplate, err)
	}
	return string(data)
}

// TestCloudInitReferencesThePlatformRepoCredentialVariables keeps
// bootstrap.tf and the template from drifting apart.
func TestCloudInitReferencesThePlatformRepoCredentialVariables(t *testing.T) {
	content := readCloudInitTemplate(t)
	for _, want := range []string{
		"${platform_repo_github_app_id}",
		"${platform_repo_github_app_installation_id}",
		"${platform_repo_github_app_private_key_b64}",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("%s does not reference %s", cloudInitTemplate, want)
		}
	}
}

// TestCloudInitAppliesThePlatformRepoCredentialBeforeTheRootApplication:
// ArgoCD must never be handed the root Application before it has a credential
// for the Platform repository.
func TestCloudInitAppliesThePlatformRepoCredentialBeforeTheRootApplication(t *testing.T) {
	content := readCloudInitTemplate(t)

	secretIdx := strings.Index(content, "kind: Secret")
	if secretIdx == -1 {
		t.Fatal("no Secret is applied by the cloud-init script")
	}
	labelIdx := strings.Index(content, "argocd.argoproj.io/secret-type: repository")
	if labelIdx == -1 {
		t.Fatal("no ArgoCD repository-credential Secret (argocd.argoproj.io/secret-type: repository) is applied")
	}
	appIdx := strings.Index(content, "kind: Application")
	if appIdx == -1 {
		t.Fatal("no root ArgoCD Application is applied")
	}

	if secretIdx > appIdx {
		t.Errorf("the credential Secret (offset %d) is applied after the root Application (offset %d); it must come first", secretIdx, appIdx)
	}
	if labelIdx > appIdx {
		t.Errorf("the repository-credential label (offset %d) is applied after the root Application (offset %d); it must come first", labelIdx, appIdx)
	}
}

// TestCloudInitRepositorySecretIDsAreQuoted: an unquoted digit string
// becomes a JSON number once kubectl converts the manifest, which the API
// server refuses for Secret.stringData. yaml.v3 would accept it, so
// TestCloudInitRepositorySecretIsWellFormedYAML cannot catch this.
func TestCloudInitRepositorySecretIDsAreQuoted(t *testing.T) {
	content := readCloudInitTemplate(t)
	for _, want := range []string{
		`githubAppID: "$PLATFORM_REPO_GITHUB_APP_ID"`,
		`githubAppInstallationID: "$PLATFORM_REPO_GITHUB_APP_INSTALLATION_ID"`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("%s does not contain the quoted form %q (an unquoted digit string here breaks kubectl apply against Secret.stringData)", cloudInitTemplate, want)
		}
	}
}

// TestCloudInitRepositorySecretIsWellFormedYAML reconstructs what the
// script's heredoc feeds to `kubectl apply`, to check that the generated
// githubAppPrivateKey block scalar is nested deeper than its sibling keys.
func TestCloudInitRepositorySecretIsWellFormedYAML(t *testing.T) {
	content := readCloudInitTemplate(t)

	re := regexp.MustCompile(`(?s)( {6}apiVersion: v1\n.*? {8}githubAppPrivateKey: \|\n)`)
	block := re.FindString(content)
	if block == "" {
		t.Fatal("could not find the repository Secret's static YAML block in the template")
	}
	dedented := dedent(t, block, 6)

	fakeKey := "-----BEGIN RSA PRIVATE KEY-----\nline-one\nline-two\n-----END RSA PRIVATE KEY-----"
	indentedKey := indent(fakeKey, 4) + "\n"

	full := dedented + indentedKey

	var doc struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name      string            `yaml:"name"`
			Namespace string            `yaml:"namespace"`
			Labels    map[string]string `yaml:"labels"`
		} `yaml:"metadata"`
		StringData struct {
			Type                    string `yaml:"type"`
			URL                     string `yaml:"url"`
			GithubAppID             string `yaml:"githubAppID"`
			GithubAppInstallationID string `yaml:"githubAppInstallationID"`
			GithubAppPrivateKey     string `yaml:"githubAppPrivateKey"`
		} `yaml:"stringData"`
	}
	if err := yaml.Unmarshal([]byte(full), &doc); err != nil {
		t.Fatalf("reconstructed Secret is not valid YAML: %v\n---\n%s\n---", err, full)
	}

	if doc.Kind != "Secret" {
		t.Errorf("kind = %q, want Secret", doc.Kind)
	}
	if doc.Metadata.Namespace != "argocd" {
		t.Errorf("metadata.namespace = %q, want argocd", doc.Metadata.Namespace)
	}
	if doc.Metadata.Labels["argocd.argoproj.io/secret-type"] != "repository" {
		t.Errorf("labels[argocd.argoproj.io/secret-type] = %q, want repository", doc.Metadata.Labels["argocd.argoproj.io/secret-type"])
	}
	if doc.StringData.Type != "git" {
		t.Errorf("stringData.type = %q, want git", doc.StringData.Type)
	}
	if doc.StringData.URL == "" {
		t.Error("stringData.url is empty")
	}
	if doc.StringData.GithubAppID == "" {
		t.Error("stringData.githubAppID is empty")
	}
	if doc.StringData.GithubAppInstallationID == "" {
		t.Error("stringData.githubAppInstallationID is empty")
	}
	// A "|" block scalar keeps one trailing newline, which a PEM file already
	// ends with.
	if want := fakeKey + "\n"; doc.StringData.GithubAppPrivateKey != want {
		t.Errorf("stringData.githubAppPrivateKey = %q, want %q (block-scalar nesting broke the key's content)", doc.StringData.GithubAppPrivateKey, want)
	}
}

// dedent strips exactly n leading spaces from every non-empty line, as
// cloud-init's "content: |" block scalar does. It fails on a shorter line,
// which would mean the block's indentation had drifted.
func dedent(t *testing.T, s string, n int) string {
	t.Helper()
	prefix := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, prefix) {
			t.Fatalf("line %d (%q) has less than %d spaces of indentation", i, line, n)
		}
		lines[i] = line[n:]
	}
	return strings.Join(lines, "\n")
}

// indent prefixes every line with n spaces, like the script's
// `sed 's/^/    /'`.
func indent(s string, n int) string {
	prefix := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// bootstrapScript returns the iidp-bootstrap script as cloud-init writes it
// to disk: the write_files block dedented, OpenTofu's ${name} variables
// replaced with a placeholder and its $${ escapes turned back into ${.
func bootstrapScript(t *testing.T) string {
	t.Helper()
	content := readCloudInitTemplate(t)
	start := strings.Index(content, "      #!/usr/bin/env bash\n")
	if start < 0 {
		t.Fatal("could not find the iidp-bootstrap script in the template")
	}
	var lines []string
	for _, line := range strings.Split(content[start:], "\n") {
		if line != "" && !strings.HasPrefix(line, "      ") {
			break
		}
		lines = append(lines, line)
	}
	script := dedent(t, strings.Join(lines, "\n"), 6)
	script = regexp.MustCompile(`(^|[^$])\$\{[a-z0-9_]+\}`).ReplaceAllString(script, "${1}placeholder")
	return strings.ReplaceAll(script, "$${", "${")
}

func TestBootstrapScriptIsValidBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(bootstrapScript(t))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

// Re-applying the stock argo-cd chart over an ArgoCD that manages itself
// would reset its customisations until it self-heals.
func TestBootstrapScriptInstallsArgoCDOnlyBeforeItManagesItself(t *testing.T) {
	script := bootstrapScript(t)
	guard := strings.Index(script, "if kubectl -n argocd get applications.argoproj.io argocd >/dev/null 2>&1; then")
	install := strings.Index(script, "helm template argocd argo-cd")
	if guard < 0 || install < 0 {
		t.Fatalf("guard at %d, install at %d; want both present", guard, install)
	}
	block := script[guard:]
	elseAt := strings.Index(block, "\nelse\n")
	fiAt := strings.Index(block, "\nfi\n")
	if elseAt < 0 || fiAt < elseAt || install < guard+elseAt || install > guard+fiAt {
		t.Errorf("helm template is not in the else branch of the argocd Application check")
	}
	if strings.Count(script, "helm template argocd argo-cd") != 1 {
		t.Error("the argo-cd chart is rendered more than once")
	}
}
