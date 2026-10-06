package application_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/appconfig"
	valuesrender "github.com/Itema-as/iidp/internal/render"
)

// Preview Environments: the ApplicationSet the CLI writes, instantiated for
// one pull request the way ArgoCD's Pull Request generator does, and the
// chart rendered from the Application that comes out: staging's values file,
// then the template's valuesObject over it.

// pullRequest is the generator's parameters for one pull request, as ArgoCD's
// ApplicationSet controller builds them (applicationset/generators/
// pull_request.go).
var pullRequest = map[string]any{
	"number":           "42",
	"branch":           "feature/checkout",
	"branch_slug":      "feature-checkout",
	"target_branch":    "main",
	"head_sha":         "0123456789abcdef0123456789abcdef01234567",
	"head_short_sha":   "01234567",
	"head_short_sha_7": "0123456",
	"title":            "A new checkout",
	"author":           "a-developer",
}

// instantiate applies params to every string and map key of the
// ApplicationSet's template with text/template and missingkey=error, as
// ArgoCD's goTemplate rendering does (applicationset/utils/utils.go,
// deeplyReplace).
func instantiate(t *testing.T, node any, params map[string]any) any {
	t.Helper()
	apply := func(s string) string {
		tmpl, err := template.New("").Option("missingkey=error").Parse(s)
		if err != nil {
			t.Fatalf("template %q: %v", s, err)
		}
		var out bytes.Buffer
		if err := tmpl.Execute(&out, params); err != nil {
			t.Fatalf("template %q: %v", s, err)
		}
		return out.String()
	}
	switch v := node.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range v {
			out[apply(k)] = instantiate(t, val, params)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, val := range v {
			out[i] = instantiate(t, val, params)
		}
		return out
	case string:
		return apply(v)
	default:
		return v
	}
}

// previewFixture writes a staging Environment with every Capability a preview
// must honour or override, through the CLI's and the Deploy gate's own
// writers, and returns the staging values file's path and the previews
// ApplicationSet.
func previewFixture(t *testing.T) (stagingValues string, applicationSet map[string]any) {
	t.Helper()
	staging := valuesrender.Environment{
		Application:           "shop",
		Environment:           "staging",
		BaseDomain:            "app.itma.no",
		Kind:                  "web-service",
		ImageRepository:       "ghcr.io/itema-as/shop",
		Size:                  "medium",
		Port:                  3000,
		ProbePath:             "/",
		PostgresEnabled:       true,
		BackupsBucket:         "itema-iidp-db-backups",
		ObjectStorageEndpoint: "https://hel1.your-objectstorage.com",
		Login:                 true,
		LoginCookieDomain:     "itma.no",
		LoginGroups:           []string{"0f3b6a4e-8c1d-4e2f-9a7b-5c6d7e8f9a0b"},
	}
	values, err := valuesrender.Values(staging)
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func([]byte) ([]byte, bool, error){
		func(v []byte) ([]byte, bool, error) {
			return valuesrender.SetImageTag(v, "fedcba9876543210fedcba9876543210fedcba98")
		},
		func(v []byte) ([]byte, bool, error) {
			return valuesrender.SetMigrationCommand(v, "npx prisma migrate deploy")
		},
		func(v []byte) ([]byte, bool, error) {
			return valuesrender.SetTasks(v, []appconfig.Task{{Name: "nightly-cleanup", Schedule: "0 3 * * *", Command: "node cleanup.js"}})
		},
		func(v []byte) ([]byte, bool, error) { return valuesrender.AddSecretName(v, "shop-staging-api-key") },
		func(v []byte) ([]byte, bool, error) {
			return valuesrender.SetDatabaseAccess(v, valuesrender.DatabaseAccess{ReadWrite: "push", ReadOnly: "pull"})
		},
		func(v []byte) ([]byte, bool, error) {
			return valuesrender.SetPasswordSecret(v, valuesrender.ReadWriteRole, "shop-staging-db-write")
		},
		func(v []byte) ([]byte, bool, error) {
			return valuesrender.SetPasswordSecret(v, valuesrender.ReadOnlyRole, "shop-staging-db-read")
		},
		// The CLI writes custom domains for prod only; a hand-edited
		// staging with one must still give previews none.
		func(v []byte) ([]byte, bool, error) { return valuesrender.AddDomain(v, "staging.shop.example.com") },
	} {
		if values, _, err = edit(values); err != nil {
			t.Fatal(err)
		}
	}
	stagingValues = filepath.Join(t.TempDir(), "staging-values.yaml")
	if err := os.WriteFile(stagingValues, values, 0o644); err != nil {
		t.Fatal(err)
	}

	chart := valuesrender.Chart{RepoURL: "ghcr.io/itema-as/charts", Name: "application", Version: "0.3.1"}
	application, err := valuesrender.ArgoCDApplication(staging, chart, "https://github.com/Itema-as/iidp-platform.git", "applications/shop/staging/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if application, _, err = valuesrender.AddKustomizeSource(application, "https://github.com/Itema-as/iidp-platform.git", "applications/shop/staging/sops"); err != nil {
		t.Fatal(err)
	}
	set, err := valuesrender.PreviewApplicationSet(valuesrender.Previews{Application: "shop", Owner: "Itema-as", Repository: "shop", Staging: application})
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(set, &applicationSet); err != nil {
		t.Fatalf("the ApplicationSet is not YAML: %v\n%s", err, set)
	}
	return stagingValues, applicationSet
}

// renderPreview instantiates the ApplicationSet's template for pullRequest
// and renders the chart the way ArgoCD renders a multi-source Helm
// Application. It returns the Application and the rendered manifests.
func renderPreview(t *testing.T) (application map[string]any, manifests string) {
	t.Helper()
	stagingValues, set := previewFixture(t)
	spec := get[map[string]any](t, set, "spec")
	application = instantiate(t, spec["template"], pullRequest).(map[string]any)

	var chartSource map[string]any
	for _, s := range get[[]any](t, application, "spec", "sources") {
		if source := s.(map[string]any); source["helm"] != nil {
			chartSource = source
		}
	}
	if chartSource == nil {
		t.Fatal("the Application has no chart source")
	}
	helm := get[map[string]any](t, chartSource, "helm")
	if files := get[[]any](t, helm, "valueFiles"); len(files) != 1 || files[0] != "$values/applications/shop/staging/values.yaml" {
		t.Fatalf("valueFiles = %v, want staging's", files)
	}
	override, err := yaml.Marshal(get[map[string]any](t, helm, "valuesObject"))
	if err != nil {
		t.Fatal(err)
	}
	overrideFile := filepath.Join(t.TempDir(), "values-object.yaml")
	if err := os.WriteFile(overrideFile, override, 0o644); err != nil {
		t.Fatal(err)
	}
	namespace := get[string](t, application, "spec", "destination", "namespace")
	out, err := helmTemplateFile(t, stagingValues, "--values", overrideFile, "--namespace", namespace)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	return application, out
}

func TestAPreviewIsItsOwnEnvironmentOfThePullRequest(t *testing.T) {
	application, _ := renderPreview(t)
	if name := get[string](t, application, "metadata", "name"); name != "shop-pr-42" {
		t.Errorf("Application name = %q, want shop-pr-42", name)
	}
	if ns := get[string](t, application, "spec", "destination", "namespace"); ns != "shop-pr-42" {
		t.Errorf("destination namespace = %q, want shop-pr-42", ns)
	}
	if got := get[map[string]any](t, application, "metadata", "labels"); got["iidp.itema.no/application"] != "shop" || got["iidp.itema.no/environment"] != "pr-42" {
		t.Errorf("Application labels = %v", got)
	}
	// The same namespace labels as every Environment's, so the guardrails bind
	// to it, and ArgoCD's tracking annotation, so the namespace goes with the
	// Application.
	metadata := get[map[string]any](t, application, "spec", "syncPolicy", "managedNamespaceMetadata")
	labels := get[map[string]any](t, metadata, "labels")
	for key, want := range valuesrender.NamespaceLabels("shop", "pr-42") {
		if labels[key] != want {
			t.Errorf("namespace label %s = %v, want %s", key, labels[key], want)
		}
	}
	if got := get[map[string]any](t, metadata, "annotations")["argocd.argoproj.io/tracking-id"]; got != "shop-pr-42:/Namespace:/shop-pr-42" {
		t.Errorf("namespace tracking-id = %v, want shop-pr-42:/Namespace:/shop-pr-42", got)
	}
	if !slices.Contains(get[[]any](t, application, "spec", "syncPolicy", "syncOptions"), any("CreateNamespace=true")) {
		t.Error("the Application does not create its namespace")
	}
	if !slices.Contains(get[[]any](t, application, "metadata", "finalizers"), any("resources-finalizer.argocd.argoproj.io")) {
		t.Error("the Application has no resources finalizer, so closing the pull request would leave its resources")
	}
}

func TestAPreviewRunsThePullRequestsImageAtTheSmallestSizeAtItsOwnAddress(t *testing.T) {
	_, manifests := renderPreview(t)
	objects := parseObjects(t, manifests)
	c := container(t, objects, "shop-pr-42")
	if image := get[string](t, c, "image"); image != "ghcr.io/itema-as/shop:"+pullRequest["head_sha"].(string) {
		t.Errorf("image = %q, want the pull request's head SHA tag", image)
	}
	for field, cpu := range map[string]string{"requests": "50m", "limits": "250m"} {
		res := get[map[string]any](t, c, "resources", field)
		if res["cpu"] != cpu || res["memory"] != "256Mi" {
			t.Errorf("resources.%s = %v, want small's %s and 256Mi (staging is medium)", field, res, cpu)
		}
	}
	if hosts := ingressHosts(t, mustObject(t, objects, "Ingress/shop-pr-42")); !slices.Equal(hosts, []string{"shop-pr-42.app.itma.no"}) {
		t.Errorf("hosts = %v, want only shop-pr-42.app.itma.no", hosts)
	}
	labels := get[map[string]any](t, mustObject(t, objects, "Deployment/shop-pr-42"), "metadata", "labels")
	if labels["iidp.itema.no/environment"] != "pr-42" {
		t.Errorf("iidp.itema.no/environment = %v, want pr-42", labels["iidp.itema.no/environment"])
	}
}

func TestAPreviewIsBehindItemaLoginWithTheApplicationsGroups(t *testing.T) {
	_, manifests := renderPreview(t)
	objects := parseObjects(t, manifests)
	annotations := get[map[string]any](t, mustObject(t, objects, "Ingress/shop-pr-42"), "metadata", "annotations")
	if got := annotations["traefik.ingress.kubernetes.io/router.middlewares"]; got != "shop-pr-42-shop-pr-42-itema-login@kubernetescrd" {
		t.Errorf("router.middlewares = %v, want the preview's own sign-in groups Middleware", got)
	}
	address := get[string](t, mustObject(t, objects, "Middleware/shop-pr-42-itema-login"), "spec", "forwardAuth", "address")
	if !strings.HasSuffix(address, "?allowed_groups=0f3b6a4e-8c1d-4e2f-9a7b-5c6d7e8f9a0b") {
		t.Errorf("forwardAuth.address = %q, want staging's sign-in group", address)
	}
}

func TestAPreviewHasNoTasksAndNoCustomDomains(t *testing.T) {
	_, manifests := renderPreview(t)
	objects := parseObjects(t, manifests)
	if cronJobs := objectsOfKind(objects, "CronJob"); len(cronJobs) != 0 {
		t.Errorf("rendered %v, want no Scheduled task in a preview", cronJobs)
	}
	if _, ok := objects["Ingress/shop-pr-42-http01"]; ok {
		t.Error("rendered an HTTP-01 Ingress, want no custom domain in a preview")
	}
	if data := mustObject(t, objects, "ConfigMap/iidp-domains")["data"]; data != nil {
		t.Errorf("iidp-domains data = %v, want no declared domains", data)
	}
}

func TestAPreviewsDatabaseHasNoBackupsAndKeepsStagingsMigration(t *testing.T) {
	_, manifests := renderPreview(t)
	objects := parseObjects(t, manifests)
	cluster := mustObject(t, objects, "Cluster/shop-pr-42-db")
	if plugins, ok := get[map[string]any](t, cluster, "spec")["plugins"]; ok {
		t.Errorf("Cluster plugins = %v, want no WAL archiving", plugins)
	}
	for _, kind := range []string{"ObjectStore", "ScheduledBackup", "ServiceAccount"} {
		if found := objectsOfKind(objects, kind); len(found) != 0 {
			t.Errorf("rendered %v, want no backup objects", found)
		}
	}
	// The only RBAC a preview renders is the database tunnel's.
	for _, kind := range []string{"Role", "RoleBinding"} {
		if found := objectsOfKind(objects, kind); !slices.Equal(found, []string{kind + "/shop-pr-42-db-tunnel"}) {
			t.Errorf("rendered %v, want only %s/shop-pr-42-db-tunnel", found, kind)
		}
	}
	for key := range objects {
		if strings.Contains(key, "final-backup") {
			t.Errorf("rendered %s, want no final backup on delete", key)
		}
	}
	job := mustObject(t, objects, "Job/shop-pr-42-migrate")
	containers := get[[]any](t, job, "spec", "template", "spec", "containers")
	if command := anyStrings(containers[0].(map[string]any)["command"]); !slices.Equal(command, []string{"sh", "-c", "npx prisma migrate deploy"}) {
		t.Errorf("the migration Job runs %v, want staging's command", command)
	}
}

func TestAPreviewGetsStagingsSecretsThroughTheSameSource(t *testing.T) {
	application, manifests := renderPreview(t)
	var paths []string
	for _, s := range get[[]any](t, application, "spec", "sources") {
		if p, ok := s.(map[string]any)["path"].(string); ok {
			paths = append(paths, p)
		}
	}
	if !slices.Equal(paths, []string{"applications/shop/staging/sops"}) {
		t.Errorf("kustomize sources = %v, want staging's sops/", paths)
	}
	c := container(t, parseObjects(t, manifests), "shop-pr-42")
	envFrom := get[[]any](t, c, "envFrom")
	if len(envFrom) != 1 || get[string](t, envFrom[0], "secretRef", "name") != "shop-staging-api-key" {
		t.Errorf("envFrom = %v, want staging's Secret shop-staging-api-key", envFrom)
	}
}

// A preview opens its database to the same developers as staging, with
// staging's password Secrets, which staging's sops/ source applies in the
// preview's namespace.
func TestAPreviewHasStagingsDatabaseAccessAndPasswords(t *testing.T) {
	_, manifests := renderPreview(t)
	objects := parseObjects(t, manifests)
	cluster := mustObject(t, objects, "Cluster/shop-pr-42-db")
	if got := annotationsOf(t, cluster); got[readWriteAnnotation] != "push" || got[readOnlyAnnotation] != "pull" {
		t.Errorf("annotations = %v, want staging's push and pull", got)
	}
	present, _ := managedRoles(t, cluster)
	roles := map[string]string{}
	for name, role := range present {
		roles[name] = get[string](t, role, "passwordSecret", "name")
	}
	if want := map[string]string{"shop_write": "shop-staging-db-write", "shop_read": "shop-staging-db-read"}; !mapsEqual(roles, want) {
		t.Errorf("managed roles = %v, want %v", roles, want)
	}
	role, binding := tunnelRole(t, objects, "shop-pr-42")
	if role == nil {
		t.Fatalf("no Role/shop-pr-42-db-tunnel; got %v", keys(objects))
	}
	if got := tunnelSecrets(t, role); !slices.Equal(got, []string{"shop-staging-db-read", "shop-staging-db-write"}) {
		t.Errorf("the database tunnel may get %v, want staging's two password Secrets", got)
	}
	if ns := get[map[string]any](t, binding, "metadata")["namespace"]; ns != nil && ns != "shop-pr-42" {
		t.Errorf("RoleBinding namespace = %v, want the preview's", ns)
	}
}

func TestAPreviewPassesKubeconformWithTheCRDSchemas(t *testing.T) {
	requireTool(t, "kubeconform")
	_, manifests := renderPreview(t)
	cmd := exec.Command("kubeconform", "-strict", "-summary", "-kubernetes-version", kubernetesVersion(t),
		"-schema-location", "default", "-schema-location", crdSchemas)
	cmd.Stdin = strings.NewReader(manifests)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubeconform: %v\n%s", err, out)
	}
	t.Logf("kubeconform: %s", strings.TrimSpace(string(out)))
}

// postgres.backups: false on its own needs neither the bucket nor the
// endpoint.
func TestPostgresWithoutBackupsRendersTheDatabaseAlone(t *testing.T) {
	objects := render(t, "preview-postgres.yaml", "--namespace", "shop-pr-7")
	for _, key := range []string{"Cluster/shop-pr-7-db", "Job/shop-pr-7-migrate", "Deployment/shop-pr-7", "Ingress/shop-pr-7"} {
		mustObject(t, objects, key)
	}
	for _, kind := range []string{"ObjectStore", "ScheduledBackup", "ServiceAccount", "Role", "RoleBinding"} {
		if found := objectsOfKind(objects, kind); len(found) != 0 {
			t.Errorf("rendered %v with postgres.backups: false", found)
		}
	}
	if _, ok := get[map[string]any](t, mustObject(t, objects, "Cluster/shop-pr-7-db"), "spec")["plugins"]; ok {
		t.Error("the Cluster archives WAL with postgres.backups: false")
	}
	if hosts := ingressHosts(t, mustObject(t, objects, "Ingress/shop-pr-7")); !slices.Equal(hosts, []string{"shop-pr-7.app.itma.no"}) {
		t.Errorf("hosts = %v, want shop-pr-7.app.itma.no", hosts)
	}
}

func TestRenderingRefusesPreviewValuesItCannotHonour(t *testing.T) {
	for _, tc := range []struct{ fixture, message string }{
		{"refuse-unknown-environment.yaml", `environment must be prod, staging or pr-<pull request number>, got "pr-007"`},
		{"refuse-preview-name-too-long.yaml", "abcdefghij-abcdefghij-abcdefghij-abcdefghij-abcdefghijk-pr-12345 is longer than the 63 characters"},
		{"refuse-postgres-backups-not-bool.yaml", "postgres.backups must be true or false, got no"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			out, err := helmTemplate(t, tc.fixture)
			if err == nil {
				t.Fatalf("rendering succeeded, want a failure:\n%s", out)
			}
			if !strings.Contains(out, tc.message) {
				t.Fatalf("error does not say %q:\n%s", tc.message, out)
			}
		})
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}
