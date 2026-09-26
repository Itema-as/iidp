package platform_test

// Tests for issue #59: cloud-init writes k3s's registries.yaml, the node's
// GHCR pull credential, before k3s starts. And for #61: it also writes the
// Deploy gate's copy of the token, a Secret manifest iidp-bootstrap applies.
//
// Two renders of the same template. renderTemplate below substitutes the
// template's ${name} values in Go and runs everywhere, CI's Go job
// included (it has no tofu). TestOpenTofuRendersRegistriesYAML renders it
// through OpenTofu itself, bootstrap.tf's yamlencode and base64encode
// included, and skips when tofu is not on PATH. Both parse the whole
// rendered cloud-config and check the same things.

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const registriesPath = "/etc/rancher/k3s/registries.yaml"

// renderTemplate does the part of templatefile() this template uses: every
// ${name} becomes vars[name], and $${ becomes a literal ${. It fails the
// test on a template directive (%{), on a ${...} that is not a plain name,
// and on a name vars does not have, so a template that outgrows this
// stand-in fails loudly instead of rendering wrong.
func renderTemplate(t *testing.T, tmpl string, vars map[string]string) string {
	t.Helper()
	if strings.Contains(tmpl, "%{") {
		t.Fatal("the template uses a %{ directive, which renderTemplate does not implement")
	}
	const escaped = "\x00ESCAPED\x00"
	s := strings.ReplaceAll(tmpl, "$${", escaped)
	ref := regexp.MustCompile(`\$\{([a-z0-9_]+)\}`)
	s = ref.ReplaceAllStringFunc(s, func(m string) string {
		name := ref.FindStringSubmatch(m)[1]
		v, ok := vars[name]
		if !ok {
			t.Errorf("the template references ${%s}, which the test does not supply", name)
		}
		return v
	})
	if strings.Contains(s, "${") {
		t.Fatalf("a ${...} other than a plain variable name is left after rendering")
	}
	return strings.ReplaceAll(s, escaped, "${")
}

type cloudConfig struct {
	WriteFiles []struct {
		Path        string `yaml:"path"`
		Permissions string `yaml:"permissions"`
		Owner       string `yaml:"owner"`
		Encoding    string `yaml:"encoding"`
		Content     string `yaml:"content"`
		Defer       bool   `yaml:"defer"`
	} `yaml:"write_files"`
	Runcmd [][]string `yaml:"runcmd"`
}

// assertRegistriesYAML checks what the issue asks of a rendered
// cloud-config: registries.yaml is written by write_files (which cloud-init
// runs before runcmd, and runcmd is what installs and starts k3s), mode
// 600, root-owned, not deferred, and holds the given credentials for
// ghcr.io in the shape the k3s documentation gives
// (configs.<registry>.auth.username/password).
func assertRegistriesYAML(t *testing.T, rendered, wantUser, wantToken string) {
	t.Helper()
	var cfg cloudConfig
	if err := yaml.Unmarshal([]byte(rendered), &cfg); err != nil {
		t.Fatalf("the rendered cloud-config is not valid YAML: %v", err)
	}
	if !strings.HasPrefix(rendered, "#cloud-config\n") {
		t.Error("the rendered user data does not start with #cloud-config")
	}
	if len(cfg.Runcmd) == 0 {
		t.Error("the rendered cloud-config has no runcmd, so nothing starts k3s")
	}

	found := 0
	for _, f := range cfg.WriteFiles {
		if f.Path != registriesPath {
			continue
		}
		found++
		if f.Permissions != "0600" {
			t.Errorf("%s permissions = %q, want \"0600\"", registriesPath, f.Permissions)
		}
		if f.Owner != "root:root" {
			t.Errorf("%s owner = %q, want root:root", registriesPath, f.Owner)
		}
		if f.Defer {
			t.Errorf("%s is deferred to cloud-init's final stage; it must be written before runcmd starts k3s", registriesPath)
		}
		content := []byte(f.Content)
		if f.Encoding == "b64" {
			var err error
			if content, err = base64.StdEncoding.DecodeString(f.Content); err != nil {
				t.Fatalf("%s content is not valid base64: %v", registriesPath, err)
			}
		} else if f.Encoding != "" {
			t.Fatalf("%s has encoding %q; the test knows only b64 and none", registriesPath, f.Encoding)
		}

		var reg struct {
			Mirrors map[string]any `yaml:"mirrors"`
			Configs map[string]struct {
				Auth struct {
					Username string `yaml:"username"`
					Password string `yaml:"password"`
				} `yaml:"auth"`
			} `yaml:"configs"`
		}
		if err := yaml.Unmarshal(content, &reg); err != nil {
			t.Fatalf("%s is not valid YAML: %v\n---\n%s", registriesPath, err, content)
		}
		ghcr, ok := reg.Configs["ghcr.io"]
		if !ok {
			t.Fatalf("%s has no configs entry for ghcr.io:\n%s", registriesPath, content)
		}
		if ghcr.Auth.Username != wantUser {
			t.Errorf("configs.ghcr.io.auth.username = %q, want %q", ghcr.Auth.Username, wantUser)
		}
		if ghcr.Auth.Password != wantToken {
			t.Errorf("configs.ghcr.io.auth.password = %q, want the token", ghcr.Auth.Password)
		}
		if len(reg.Configs) != 1 || len(reg.Mirrors) != 0 {
			t.Errorf("%s configures more than ghcr.io's credentials:\n%s", registriesPath, content)
		}
	}
	if found != 1 {
		t.Fatalf("write_files has %d entries for %s, want 1", found, registriesPath)
	}

	// The token must reach the node only through that file, never through
	// the bootstrap script, whose output is teed to a log.
	script := strings.SplitN(rendered, "/usr/local/sbin/iidp-bootstrap", 2)
	if len(script) == 2 && strings.Contains(script[1], wantToken) {
		t.Error("the token appears in the iidp-bootstrap script")
	}
}

const gatePullSecretPath = "/etc/iidp/ghcr-pull-token.yaml"

// assertGatePullSecret checks the Deploy gate's copy of the token (#61):
// cloud-init writes the manifest of Secret argocd/ghcr-pull-token, with
// the given username and token under the keys the gate's chart mounts, to
// a root-only file, and iidp-bootstrap applies that file before the root
// Application, so the gate finds the Secret when ArgoCD first installs it.
func assertGatePullSecret(t *testing.T, rendered, wantUser, wantToken string) {
	t.Helper()
	var cfg cloudConfig
	if err := yaml.Unmarshal([]byte(rendered), &cfg); err != nil {
		t.Fatalf("the rendered cloud-config is not valid YAML: %v", err)
	}
	found := 0
	for _, f := range cfg.WriteFiles {
		if f.Path != gatePullSecretPath {
			continue
		}
		found++
		if f.Permissions != "0600" || f.Owner != "root:root" {
			t.Errorf("%s is %s %s, want 0600 root:root: it holds the token", gatePullSecretPath, f.Permissions, f.Owner)
		}
		if f.Encoding != "b64" {
			t.Fatalf("%s has encoding %q, want b64", gatePullSecretPath, f.Encoding)
		}
		content, err := base64.StdEncoding.DecodeString(f.Content)
		if err != nil {
			t.Fatalf("%s content is not valid base64: %v", gatePullSecretPath, err)
		}
		var secret struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
			Metadata   struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
			StringData map[string]string `yaml:"stringData"`
		}
		if err := yaml.Unmarshal(content, &secret); err != nil {
			t.Fatalf("%s is not valid YAML: %v\n---\n%s", gatePullSecretPath, err, content)
		}
		if secret.APIVersion != "v1" || secret.Kind != "Secret" || secret.Metadata.Name != "ghcr-pull-token" || secret.Metadata.Namespace != "argocd" {
			t.Errorf("%s is %s %s %s/%s, want the v1 Secret argocd/ghcr-pull-token the gate's chart mounts", gatePullSecretPath, secret.APIVersion, secret.Kind, secret.Metadata.Namespace, secret.Metadata.Name)
		}
		if len(secret.StringData) != 2 || secret.StringData["username"] != wantUser || secret.StringData["token"] != wantToken {
			t.Errorf("%s stringData = %v, want exactly username %q and the token", gatePullSecretPath, secret.StringData, wantUser)
		}
	}
	if found != 1 {
		t.Fatalf("write_files has %d entries for %s, want 1", found, gatePullSecretPath)
	}

	apply := strings.Index(rendered, "kubectl apply --server-side -f "+gatePullSecretPath)
	root := strings.Index(rendered, "name: platform\n")
	if apply < 0 || root < 0 || apply > root {
		t.Errorf("iidp-bootstrap must apply %s before the root Application (apply at %d, root Application at %d)", gatePullSecretPath, apply, root)
	}
}

func TestCloudInitWritesRegistriesYAMLBeforeK3sStarts(t *testing.T) {
	const user, token = "platform-admin", "ghp_TestToken0123456789abcdefABCDEF0123"
	registries := "configs:\n  ghcr.io:\n    auth:\n      username: " + user + "\n      password: " + token + "\n"
	gateSecret := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: ghcr-pull-token\n  namespace: argocd\ntype: Opaque\nstringData:\n  username: " + user + "\n  token: " + token + "\n"
	rendered := renderTemplate(t, readCloudInitTemplate(t), map[string]string{
		"k3s_version":                              "v1.36.4+k3s1",
		"argocd_chart_version":                     "10.9.2",
		"helm_version":                             "v3.22.0",
		"helm_sha256_linux_amd64":                  strings.Repeat("0", 64),
		"platform_repo_url":                        "https://github.com/Itema-as/iidp-platform.git",
		"platform_repo_bootstrap_path":             "bootstrap",
		"platform_repo_github_app_id":              "123456",
		"platform_repo_github_app_installation_id": "78901234",
		"platform_repo_github_app_private_key_b64": base64.StdEncoding.EncodeToString([]byte("-----BEGIN RSA PRIVATE KEY-----\nx\n-----END RSA PRIVATE KEY-----\n")),
		"registries_yaml_b64":                      base64.StdEncoding.EncodeToString([]byte(registries)),
		"ghcr_pull_secret_yaml_b64":                base64.StdEncoding.EncodeToString([]byte(gateSecret)),
		"audit_policy_yaml_b64":                    base64.StdEncoding.EncodeToString(readAuditPolicy(t)),
	})
	assertRegistriesYAML(t, rendered, user, token)
	assertGatePullSecret(t, rendered, user, token)
	assertAuditLog(t, rendered)
}

// TestOpenTofuRendersRegistriesYAML renders the real user data: bootstrap.tf,
// variables.tf and the template, copied into a scratch root with no
// provider and no backend, applied with local state against fixture
// variables, and read back as an output. This covers what the Go render
// cannot: bootstrap.tf's yamlencode and base64encode, and the variables'
// validation. Skipped when tofu is not installed (CI's Go job); set
// IIDP_REQUIRE_TOFU=1 to fail instead.
func TestOpenTofuRendersRegistriesYAML(t *testing.T) {
	tofu, err := exec.LookPath("tofu")
	if err != nil {
		if os.Getenv("IIDP_REQUIRE_TOFU") == "1" {
			t.Fatal("tofu is not on PATH and IIDP_REQUIRE_TOFU=1")
		}
		t.Skip("tofu is not on PATH")
	}

	dir := t.TempDir()
	for _, f := range []string{"bootstrap.tf", "variables.tf", cloudInitTemplate, auditPolicyFile} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(dir, "render.tf"), `
output "user_data" {
  value     = local.user_data
  sensitive = true
}
`)
	const user, token = "platform-admin", "ghp_TestToken0123456789abcdefABCDEF0123"
	writeFile(t, filepath.Join(dir, "fixture.tfvars"), `
ssh_public_key                           = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFake admin@example"
platform_repo_github_app_id              = 123456
platform_repo_github_app_installation_id = 78901234
platform_repo_github_app_private_key     = <<EOT
-----BEGIN RSA PRIVATE KEY-----
x
-----END RSA PRIVATE KEY-----
EOT
ghcr_pull_username = "`+user+`"
ghcr_pull_token    = "`+token+`"
`)

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(tofu, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_INPUT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("tofu %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	run("init", "-input=false", "-no-color")
	run("apply", "-auto-approve", "-input=false", "-no-color", "-var-file=fixture.tfvars")
	cmd := exec.Command(tofu, "output", "-raw", "user_data")
	cmd.Dir = dir
	rendered, err := cmd.Output()
	if err != nil {
		t.Fatalf("tofu output -raw user_data: %v", err)
	}
	assertRegistriesYAML(t, string(rendered), user, token)
	assertGatePullSecret(t, string(rendered), user, token)
	assertAuditLog(t, string(rendered))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
