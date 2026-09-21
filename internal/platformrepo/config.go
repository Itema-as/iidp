// Package platformrepo reads and writes the Platform repository: the
// Platform-wide settings in platform.yaml and, per Application, one
// directory per Environment. docs/platform-repository.md describes the
// layout.
package platformrepo

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/platform"
)

// ConfigFile is the name of the Platform settings file at the root of the
// Platform repository.
const ConfigFile = "platform.yaml"

// DefaultChartRepository is the OCI reference of the generic chart when
// platform.yaml does not name one: where the release workflow pushes it.
var DefaultChartRepository = "oci://" + platform.Registry + "/charts/application"

// Config is what the CLI reads from platform.yaml. Fields other tickets
// add to the file are ignored rather than rejected.
type Config struct {
	// BaseDomain is the Platform base domain; prod addresses are
	// <name>.<BaseDomain>. Required.
	BaseDomain string `yaml:"baseDomain"`
	// ChartVersion is the version of the generic chart new Environments are
	// pinned to. Required.
	ChartVersion string `yaml:"chartVersion"`
	// ChartRepository is the OCI reference of the generic chart, for example
	// oci://ghcr.io/itema-as/charts/application. Defaults to
	// DefaultChartRepository.
	ChartRepository string `yaml:"chartRepository"`
	// ArgoCDURL is where developers look at their Application's status.
	ArgoCDURL string `yaml:"argocdURL"`
	// GrafanaURL is where developers look at their Application's logs.
	GrafanaURL string `yaml:"grafanaURL"`
}

// LoadConfig reads platform.yaml from a clone of the Platform repository.
func LoadConfig(dir string) (Config, error) {
	data, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if err != nil {
		return Config{}, fmt.Errorf("%s has no %s: %w", platform.Repository, ConfigFile, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("%s in %s: %w", ConfigFile, platform.Repository, err)
	}
	if cfg.BaseDomain == "" {
		return Config{}, fmt.Errorf("%s in %s sets no baseDomain", ConfigFile, platform.Repository)
	}
	if cfg.ChartVersion == "" {
		return Config{}, fmt.Errorf("%s in %s sets no chartVersion", ConfigFile, platform.Repository)
	}
	if cfg.ChartRepository == "" {
		cfg.ChartRepository = DefaultChartRepository
	}
	if _, _, err := cfg.Chart(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Chart splits ChartRepository into the registry path ArgoCD wants as
// repoURL (without the oci:// scheme) and the chart name.
func (c Config) Chart() (repoURL, name string, err error) {
	ref, ok := strings.CutPrefix(c.ChartRepository, "oci://")
	if !ok {
		return "", "", fmt.Errorf("chartRepository %q in %s must start with oci://", c.ChartRepository, ConfigFile)
	}
	repoURL, name = path.Split(ref)
	repoURL = strings.TrimSuffix(repoURL, "/")
	if repoURL == "" || name == "" {
		return "", "", errors.New("chartRepository in " + ConfigFile + " must be oci://<registry>/<path>/<chart>")
	}
	return repoURL, name, nil
}
