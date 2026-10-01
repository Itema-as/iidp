package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/platformrepo"
	"github.com/Itema-as/iidp/internal/render"
)

// The fixture's previews ApplicationSet must be exactly what the CLI writes,
// so the kind test exercises the CLI's ApplicationSet, not a hand-made one.
func TestFixturePreviewsAreTheCLIsApplicationSet(t *testing.T) {
	root := filepath.Join("fixtures", "platform-repo")
	cfg, err := platformrepo.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	binding, ok, err := platformrepo.ReadRepositoryBinding(root, "notes")
	if err != nil || !ok {
		t.Fatalf("notes's binding: ok = %v, err = %v", ok, err)
	}
	owner, name, _ := strings.Cut(binding.Repository, "/")
	staging, err := os.ReadFile(filepath.Join(root, "applications", "notes", "staging", "application.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := render.PreviewApplicationSet(render.Previews{Application: "notes", Owner: owner, Repository: name, GitHubAPI: cfg.GitHubAPI, Staging: staging})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(platformrepo.PreviewsPath("notes"))))
	if err != nil {
		t.Fatal(err)
	}
	var gotDoc, wantDoc any
	if err := yaml.Unmarshal(got, &gotDoc); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(want, &wantDoc); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotDoc, wantDoc) {
		t.Errorf("%s is not what the CLI writes; the CLI writes:\n%s", platformrepo.PreviewsPath("notes"), want)
	}
}

// The hand-written fixture Environments' namespace labels, which the
// guardrails select on, must be exactly the CLI's, or the kind test proves
// the guardrails against namespaces the Platform never has.
func TestFixtureNamespacesCarryTheCLIsLabels(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("fixtures", "platform-repo", "applications", "*", "*", "application.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no fixture Environments found")
	}
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var app struct {
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Spec struct {
				SyncPolicy struct {
					ManagedNamespaceMetadata struct {
						Labels map[string]string `yaml:"labels"`
					} `yaml:"managedNamespaceMetadata"`
				} `yaml:"syncPolicy"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(data, &app); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		want := render.NamespaceLabels(app.Metadata.Labels["iidp.itema.no/application"], app.Metadata.Labels["iidp.itema.no/environment"])
		if got := app.Spec.SyncPolicy.ManagedNamespaceMetadata.Labels; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: managedNamespaceMetadata labels = %v, want the CLI's %v", path, got, want)
		}
	}
}
