package application_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/appconfig"
	valuesrender "github.com/Itema-as/iidp/internal/render"
)

// An Environment the deploy workflow has not written an image into yet
// (image.tag: "", what the CLI writes) renders nothing, so ArgoCD shows it
// Synced and Healthy instead of a comparison error; the first tag renders
// the whole Environment. See
// docs/implementation-notes/47-unreleased-environment.md.

// fixtures lists the testdata files, split into those rendered successfully
// and the refuse-* ones rendering must fail on.
func fixtures(t *testing.T) (renderable, refused []string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		name := filepath.Base(path)
		if strings.HasPrefix(name, "refuse-") {
			refused = append(refused, name)
		} else {
			renderable = append(renderable, name)
		}
	}
	return renderable, refused
}

// renderFile renders a values file the test wrote itself.
func renderFile(t *testing.T, valuesFile string) map[string]object {
	t.Helper()
	out, err := helmTemplateFile(t, valuesFile)
	if err != nil {
		t.Fatalf("helm template %s: %v\n%s", valuesFile, err, out)
	}
	return parseObjects(t, out)
}

// writeValues writes data to a values file in a fresh temporary directory.
func writeValues(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// refusal returns the message of a failed helm template, without the
// "execution error at (<template>:<line>:<col>): " prefix, which names
// whichever template Helm happened to render first.
func refusal(out string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if _, message, found := strings.Cut(line, "): "); found {
		return message
	}
	return line
}

func TestUnreleasedEnvironmentRendersNothing(t *testing.T) {
	// Every Capability that brings objects of its own is on in this
	// fixture: Postgres with a migration (the Cluster, ObjectStore,
	// ScheduledBackup, migration Job and the final-backup PreDelete hook
	// with its RBAC), a wildcard and a foreign custom domain (both
	// Ingresses) and a Secret.
	for _, namespace := range []string{"", "shop-prod"} {
		t.Run("namespace="+namespace, func(t *testing.T) {
			var args []string
			if namespace != "" {
				args = []string{"--namespace", namespace}
			}
			objects := render(t, "unreleased-prod.yaml", args...)
			if len(objects) != 0 {
				t.Errorf("rendered %v for an Environment without an image, want nothing", keys(objects))
			}
		})
	}
}

func TestEveryFixtureRendersNothingWithoutATag(t *testing.T) {
	renderable, _ := fixtures(t)
	for _, fixture := range renderable {
		// An empty tag is what the CLI writes; a tag removed altogether
		// (null) is treated the same, since the chart's default is empty.
		for _, unset := range [][]string{{"--set-string", "image.tag="}, {"--set", "image.tag=null"}} {
			t.Run(fixture+"/"+strings.Join(unset, " "), func(t *testing.T) {
				if objects := render(t, fixture, unset...); len(objects) != 0 {
					t.Errorf("rendered %v without an image tag, want nothing", keys(objects))
				}
			})
		}
	}
}

func TestUnreleasedEnvironmentIsStillRefusedForBrokenValues(t *testing.T) {
	_, refused := fixtures(t)
	if len(refused) == 0 {
		t.Fatal("no refuse-* fixtures found")
	}
	for _, fixture := range refused {
		t.Run(fixture, func(t *testing.T) {
			released, err := helmTemplate(t, fixture)
			if err == nil {
				t.Fatalf("the fixture renders with its own tag; a refuse-* fixture must fail:\n%s", released)
			}
			unreleased, err := helmTemplate(t, fixture, "--set-string", "image.tag=")
			if err == nil {
				t.Fatalf("rendering succeeded without an image tag, want the same refusal as with one (%q):\n%s", refusal(released), unreleased)
			}
			if got, want := refusal(unreleased), refusal(released); got != want {
				t.Errorf("without an image tag the refusal is %q, want the same as with one, %q", got, want)
			}
		})
	}
}

func TestEveryTemplateHonoursTheReleasedGate(t *testing.T) {
	// The rendered tests above only see the templates a fixture switches
	// on. This one names a new template that forgets the gate, before an
	// Environment without an image renders it into a comparison error, or
	// worse, a lone object.
	templates, err := filepath.Glob(filepath.Join("templates", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range templates {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `include "application.released" .`) {
			t.Errorf("%s renders without checking application.released; wrap its objects in the gate", path)
		}
	}
}

// releasedObjects is what the prod Environment of the Application in
// TestReleasingAnEnvironmentRendersIt renders once it has an image.
var releasedObjects = []string{
	"ConfigMap/iidp-domains",
	"CronJob/shop-nightly-cleanup",
	"Deployment/shop",
	"Ingress/shop",
	"Ingress/shop-http01",
	"Job/shop-final-backup",
	"Job/shop-migrate",
	"ObjectStore/shop-db",
	"Role/shop-final-backup",
	"RoleBinding/shop-final-backup",
	"ScheduledBackup/shop-db",
	"Service/shop",
	"ServiceAccount/shop-final-backup",
	"Cluster/shop-db",
}

func TestReleasingAnEnvironmentRendersIt(t *testing.T) {
	// The prod values iidp app create --staging --postgres --domain writes
	// (internal/platformrepo's writeEnvironment builds the same
	// render.Environment), then the edit iidp ci set-image makes on the
	// first v* tag. Rendering the CLI's own output, rather than a fixture,
	// keeps the two sides of the contract from drifting apart.
	created, err := valuesrender.Values(valuesrender.Environment{
		Application:           "shop",
		Environment:           "prod",
		BaseDomain:            "app.itma.no",
		Kind:                  "web-service",
		ImageRepository:       "ghcr.io/itema-as/shop",
		Size:                  "small",
		Port:                  3000,
		ProbePath:             "/",
		Domains:               []string{"butikk.app.itma.no", "shop.example.com"},
		PostgresEnabled:       true,
		MigrationCommand:      "npx prisma migrate deploy",
		BackupsBucket:         "itema-iidp-db-backups",
		ObjectStorageEndpoint: "https://hel1.your-objectstorage.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if objects := renderFile(t, writeValues(t, created)); len(objects) != 0 {
		t.Fatalf("the Environment iidp app create wrote rendered %v before its first image, want nothing", keys(objects))
	}

	released, _, err := valuesrender.SetImageTag(created, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	// The Deploy gate writes the deployed commit's Scheduled tasks in the
	// same edit.
	released, _, err = valuesrender.SetTasks(released, []appconfig.Task{{Name: "nightly-cleanup", Schedule: "0 3 * * *", Command: "node scripts/cleanup.js"}})
	if err != nil {
		t.Fatal(err)
	}
	objects := renderFile(t, writeValues(t, released))
	want := slices.Sorted(slices.Values(releasedObjects))
	if got := keys(objects); !slices.Equal(got, want) {
		t.Fatalf("after the first image rendered %v, want %v", got, want)
	}
	if image := get[string](t, container(t, objects, "shop"), "image"); image != "ghcr.io/itema-as/shop:1.0.0" {
		t.Errorf("image = %q, want ghcr.io/itema-as/shop:1.0.0", image)
	}
	migrate := get[[]any](t, mustObject(t, objects, "Job/shop-migrate"), "spec", "template", "spec", "containers")
	if image := get[string](t, migrate[0], "image"); image != "ghcr.io/itema-as/shop:1.0.0" {
		t.Errorf("migration image = %q, want ghcr.io/itema-as/shop:1.0.0", image)
	}
	task := get[[]any](t, mustObject(t, objects, "CronJob/shop-nightly-cleanup"), "spec", "jobTemplate", "spec", "template", "spec", "containers")
	if image := get[string](t, task[0], "image"); image != "ghcr.io/itema-as/shop:1.0.0" {
		t.Errorf("task image = %q, want ghcr.io/itema-as/shop:1.0.0", image)
	}
}

func TestAddedStagingEnvironmentIsUnreleasedUntilItsFirstImage(t *testing.T) {
	// iidp app add-capability --staging copies a released prod's values
	// with the tag reset to "": staging has no image until the next push to
	// main, and must render nothing until then, not prod's image.
	prod, err := valuesrender.Values(valuesrender.Environment{
		Application:     "shop",
		Environment:     "prod",
		BaseDomain:      "app.itma.no",
		Kind:            "web-service",
		ImageRepository: "ghcr.io/itema-as/shop",
		Size:            "small",
		Port:            3000,
		ProbePath:       "/",
	})
	if err != nil {
		t.Fatal(err)
	}
	prod, _, err = valuesrender.SetImageTag(prod, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := keys(renderFile(t, writeValues(t, prod))); !slices.Equal(got, []string{"ConfigMap/iidp-domains", "Deployment/shop", "Ingress/shop", "Service/shop"}) {
		t.Fatalf("released prod rendered %v", got)
	}

	staging, _, err := valuesrender.CopyValuesForStaging(prod)
	if err != nil {
		t.Fatal(err)
	}
	if objects := renderFile(t, writeValues(t, staging)); len(objects) != 0 {
		t.Fatalf("the new staging Environment rendered %v before its first image, want nothing", keys(objects))
	}

	staging, _, err = valuesrender.SetImageTag(staging, "4f1c2d9e8b7a6f5e4d3c2b1a0f9e8d7c6b5a4f3e")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ConfigMap/iidp-domains", "Deployment/shop-staging", "Ingress/shop-staging", "Service/shop-staging"}
	if got := keys(renderFile(t, writeValues(t, staging))); !slices.Equal(got, want) {
		t.Fatalf("staging after its first image rendered %v, want %v", got, want)
	}
}
