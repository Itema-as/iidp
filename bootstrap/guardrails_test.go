package bootstrap_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/render"
)

// guardrailPolicies are the four ValidatingAdmissionPolicies, one per rule,
// and the resource each one checks.
var guardrailPolicies = map[string]string{
	"iidp-images":        "pods",
	"iidp-limits":        "pods",
	"iidp-service-types": "services",
	"iidp-ingress-hosts": "ingresses",
}

// The guardrails Application follows the bootstrap pin like platform-tls,
// and hands the component the base domain, the actions and the extra
// images from platform.yaml.
func TestGuardrailsFollowTheBootstrapAndThePlatformValues(t *testing.T) {
	apps := renderApplications(t, "--values", fixture,
		"--set", "bootstrap.repoURL=https://example.test/iidp.git",
		"--set", "bootstrap.targetRevision=v9.9.9")
	app, ok := apps["guardrails"]
	if !ok {
		t.Fatalf("no guardrails Application in %v", keys(apps))
	}
	for path, want := range map[string]string{
		"repoURL":        "https://example.test/iidp.git",
		"targetRevision": "v9.9.9",
		"path":           "bootstrap/components/guardrails",
	} {
		if got := get[string](t, app, "spec", "source", path); got != want {
			t.Errorf("guardrails source %s = %q, want %q", path, got, want)
		}
	}
	values := get[object](t, app, "spec", "source", "helm", "valuesObject")
	if got := get[string](t, values, "baseDomain"); got != "app.example.test" {
		t.Errorf("baseDomain = %q, want the fixture's", got)
	}
	if got := fmt.Sprint(get[[]any](t, values, "validationActions")); got != "[Warn Audit]" {
		t.Errorf("validationActions = %s, want [Warn Audit] until the rollout switches them to Deny", got)
	}
	if got := fmt.Sprint(get[[]any](t, values, "extraAllowedImages")); got != "[docker.io/library/nginx]" {
		t.Errorf("extraAllowedImages = %s, want the fixture's nginx", got)
	}

	// The Platform's own values add no image, and the one-line switch to
	// Deny reaches the component.
	values = get[object](t, renderApplications(t, "--values", "values.yaml",
		"--set", "guardrails.validationActions={Deny,Audit}")["guardrails"], "spec", "source", "helm", "valuesObject")
	if _, has := values["extraAllowedImages"]; has {
		t.Errorf("the Platform's values pass extraAllowedImages %v; only a test Platform adds any", values["extraAllowedImages"])
	}
	if got := fmt.Sprint(get[[]any](t, values, "validationActions")); got != "[Deny Audit]" {
		t.Errorf("validationActions = %s, want [Deny Audit]", got)
	}
}

// The component renders the four policies and a binding for each, all
// with Warn and Audit, all on Application namespaces only, rendered with
// exactly the values the bootstrap's Application passes it.
func TestGuardrailsComponentRendersFourPoliciesBoundToApplicationNamespaces(t *testing.T) {
	objects := renderGuardrails(t, fixture)
	for name, resource := range guardrailPolicies {
		policy, ok := objects["ValidatingAdmissionPolicy/"+name]
		if !ok {
			t.Errorf("no ValidatingAdmissionPolicy/%s in %v", name, keys(objects))
			continue
		}
		if got := get[string](t, policy, "spec", "failurePolicy"); got != "Fail" {
			t.Errorf("%s failurePolicy = %q, want Fail", name, got)
		}
		rules := get[[]any](t, policy, "spec", "matchConstraints", "resourceRules")
		if len(rules) != 1 {
			t.Fatalf("%s has %d resource rules, want 1", name, len(rules))
		}
		if got := fmt.Sprint(rules[0].(object)["resources"]); got != "["+resource+"]" {
			t.Errorf("%s matches %s, want [%s]", name, got, resource)
		}
		if got := fmt.Sprint(rules[0].(object)["operations"]); got != "[CREATE UPDATE]" {
			t.Errorf("%s operations = %s, want [CREATE UPDATE]", name, got)
		}

		binding, ok := objects["ValidatingAdmissionPolicyBinding/"+name]
		if !ok {
			t.Errorf("no ValidatingAdmissionPolicyBinding/%s in %v", name, keys(objects))
			continue
		}
		if got := get[string](t, binding, "spec", "policyName"); got != name {
			t.Errorf("binding %s binds policy %q", name, got)
		}
		if got := fmt.Sprint(get[[]any](t, binding, "spec", "validationActions")); got != "[Warn Audit]" {
			t.Errorf("binding %s validationActions = %s, want [Warn Audit]", name, got)
		}
		selector := get[object](t, binding, "spec", "matchResources", "namespaceSelector")
		if _, has := selector["matchLabels"]; has {
			t.Errorf("binding %s selects namespaces by matchLabels too: %v", name, selector)
		}
		exprs := get[[]any](t, selector, "matchExpressions")
		if len(exprs) != 1 {
			t.Fatalf("binding %s has %d namespace expressions, want 1: %v", name, len(exprs), exprs)
		}
		// The label the CLI writes on every Environment's namespace and
		// nothing puts on a Platform namespace.
		if got := fmt.Sprint(exprs[0]); got != fmt.Sprint(object{"key": render.ApplicationNamespaceLabel, "operator": "Exists"}) {
			t.Errorf("binding %s namespace selector = %s, want %s Exists", name, got, render.ApplicationNamespaceLabel)
		}
	}
	if len(objects) != 2*len(guardrailPolicies) {
		t.Errorf("the component renders %v, want exactly the four policies and their bindings", keys(objects))
	}
}

// The image policy allows ghcr.io/itema-as/, the images the chart and
// CloudNativePG run, and a test Platform's extras, as a CEL list; the
// Ingress policy knows the base domain and finds the declared domains in
// the ConfigMap the application chart renders.
func TestGuardrailsComponentCarriesThePlatformsValues(t *testing.T) {
	objects := renderGuardrails(t, fixture)

	allowed := variable(t, objects["ValidatingAdmissionPolicy/iidp-images"], "allowed")
	for _, want := range []string{
		"'ghcr.io/itema-as/'",
		"'ghcr.io/cloudnative-pg/cloudnative-pg'",
		"'ghcr.io/cloudnative-pg/postgresql'",
		"'ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar'",
		"'docker.io/bitnami/kubectl'",
		"'quay.io/jetstack/cert-manager-acmesolver'",
		"'docker.io/library/nginx'",
	} {
		if !strings.Contains(allowed, want) {
			t.Errorf("allowed images %s lack %s", allowed, want)
		}
	}
	// The final Backup hook's image is pinned in the application chart;
	// the policy must allow the repository it names.
	helpers, err := os.ReadFile("../chart/application/templates/_helpers.tpl")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(helpers), "docker.io/bitnami/kubectl@sha256:") {
		t.Errorf("the application chart no longer runs docker.io/bitnami/kubectl@sha256:...; update allowedImages in components/guardrails/values.yaml")
	}

	ingress := objects["ValidatingAdmissionPolicy/iidp-ingress-hosts"]
	if got := variable(t, ingress, "suffix"); got != "'.app.example.test'" {
		t.Errorf("Ingress policy suffix = %s, want '.app.example.test'", got)
	}
	if got := get[string](t, ingress, "spec", "paramKind", "kind"); got != "ConfigMap" {
		t.Errorf("Ingress policy paramKind = %q, want ConfigMap", got)
	}
	binding := objects["ValidatingAdmissionPolicyBinding/iidp-ingress-hosts"]
	paramRef := get[object](t, binding, "spec", "paramRef")
	if _, has := paramRef["namespace"]; has {
		t.Errorf("paramRef names a namespace; without one the API server looks in the Ingress's own")
	}
	// Deny would refuse every Ingress in a namespace without the ConfigMap
	// even while the actions are only Warn and Audit.
	if got := get[string](t, paramRef, "parameterNotFoundAction"); got != "Allow" {
		t.Errorf("parameterNotFoundAction = %q, want Allow", got)
	}
	// The ConfigMap the application chart renders for a released
	// Environment is the one the binding looks for.
	name := get[string](t, paramRef, "name")
	chart := parseObjects(t, helmTemplate(t, "../chart/application", "--values", "../chart/application/testdata/custom-domains-mixed.yaml"))
	if _, ok := chart["ConfigMap/"+name]; !ok {
		t.Errorf("the application chart renders no ConfigMap/%s; it renders %v", name, keys(chart))
	}
}

// Deny with Warn, which the API server refuses, and an image entry that
// could break out of its CEL string are refused at render time.
func TestGuardrailsComponentRefusesBadValues(t *testing.T) {
	requireHelm(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--set", "validationActions={Deny,Warn}"}, "Deny and Warn cannot be used together"},
		{[]string{"--set", "validationActions={Block}"}, `"Block" is not one of Deny, Warn, Audit`},
		{[]string{"--set", "extraAllowedImages={evil' || true || '}"}, "must be lowercase letters"},
	} {
		out, err := exec.Command("helm", append([]string{"template", "t", "components/guardrails"}, tc.args...)...).CombinedOutput()
		if err == nil || !strings.Contains(string(out), tc.want) {
			t.Errorf("helm template %v: err = %v, output:\n%s\nwant a refusal containing %q", tc.args, err, out, tc.want)
		}
	}
}

// renderGuardrails renders components/guardrails with the valuesObject the
// bootstrap's guardrails Application passes it for platformValues.
func renderGuardrails(t *testing.T, platformValues string) map[string]object {
	t.Helper()
	app := renderApplications(t, "--values", platformValues)["guardrails"]
	values, err := yaml.Marshal(get[object](t, app, "spec", "source", "helm", "valuesObject"))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(file, values, 0o600); err != nil {
		t.Fatal(err)
	}
	return parseObjects(t, helmTemplate(t, "components/guardrails", "--values", file))
}

// variable returns the expression of a policy's named variable.
func variable(t *testing.T, policy object, name string) string {
	t.Helper()
	var names []string
	for _, v := range get[[]any](t, policy, "spec", "variables") {
		m := v.(object)
		if m["name"] == name {
			return fmt.Sprint(m["expression"])
		}
		names = append(names, fmt.Sprint(m["name"]))
	}
	t.Fatalf("no variable %q, only %v", name, names)
	return ""
}
