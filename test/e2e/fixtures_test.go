package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/render"
)

// The fixture Platform repository's Environments are hand-written in the
// shape the CLI writes; their namespace labels, which the guardrails
// select Application namespaces by (#90), must be exactly the CLI's, or
// the kind test proves the guardrails against namespaces the Platform
// never has. Runs without the e2e tag, so a drift fails go test ./....
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
