// Package platform_test exercises infra/platform's cloud-init template as
// plain text. OpenTofu's templatefile() has no practical Go equivalent
// worth reproducing here: reimplementing HCL template syntax (including
// the doubled-dollar escaping this file's own header comment explains) to
// get a byte-for-byte render would be a second template engine to keep in
// sync with OpenTofu's own, for a test that `tofu validate` (CI already
// runs it, see infra/README.md) already covers from the OpenTofu side. The
// trade-off accepted here instead: read the template as text and check the
// two things issue #41 is actually about -- that the three new variables
// are genuinely referenced, and that the credential they render into is
// applied before the root Application and produces syntactically valid
// YAML once indentation is accounted for -- without asserting on anything
// templatefile() itself is responsible for (quoting, escaping the doubled
// dollar). A real render is exercised by `tofu validate` and, eventually,
// a real `tofu apply` against a Hetzner project.
package platform_test

import (
	"os"
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

// TestCloudInitReferencesThePlatformRepoCredentialVariables guards against
// the OpenTofu side (bootstrap.tf) and the cloud-init side of the wiring
// drifting apart: every variable bootstrap.tf now passes into
// templatefile() must actually be read somewhere in the rendered script.
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

// TestCloudInitAppliesThePlatformRepoCredentialBeforeTheRootApplication is
// the acceptance criterion itself: ArgoCD must never be handed the root
// Application (which points it at the private Platform repository) before
// it already has a credential for that repository.
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

// TestCloudInitRepositorySecretIDsAreQuoted guards a real Kubernetes
// footgun that TestCloudInitRepositorySecretIsWellFormedYAML cannot catch:
// yaml.v3 happily converts an unquoted numeric scalar into a Go string
// field, but `kubectl apply` does not have that luxury -- Secret.stringData
// is map[string]string, and an unquoted digit string in YAML becomes a
// JSON number once kubectl converts the manifest, which the API server's
// decoder then refuses to unmarshal into a string field. Both bash
// substitutions must stay quoted in the template's own source text.
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
// script's own heredoc feeds to `kubectl apply` once bash has run it: the
// static lines from the template (dedented the way cloud-init's write_files
// block scalar dedents them) plus a stand-in for the private key line,
// indented the way the script's own `sed 's/^/    /'` indents it. This is
// the one property a pure text-substring check cannot see -- that the
// dynamically-generated githubAppPrivateKey block scalar is actually nested
// deeper than its sibling keys -- so it is worth reconstructing rather than
// leaving to a real `tofu apply` to discover.
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
	// YAML's "|" block scalar keeps one trailing newline by default
	// ("clip" chomping); that is exactly what a PEM file already ends
	// with, so it is not stripped here.
	if want := fakeKey + "\n"; doc.StringData.GithubAppPrivateKey != want {
		t.Errorf("stringData.githubAppPrivateKey = %q, want %q (block-scalar nesting broke the key's content)", doc.StringData.GithubAppPrivateKey, want)
	}
}

// dedent strips exactly n leading spaces from every non-empty line,
// matching the indentation cloud-init's write_files "content: |" block
// scalar strips (established by the block's first line) before writing
// the script to disk. It fails the test rather than silently truncating a
// shorter line, since that would mean this block's own indentation had
// drifted from the rest of the script.
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

// indent prefixes every line with n spaces, the same transform the
// script's own `sed 's/^/    /'` applies to the decoded private key.
func indent(s string, n int) string {
	prefix := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}
