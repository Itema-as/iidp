package render_test

import (
	"strings"
	"testing"

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
