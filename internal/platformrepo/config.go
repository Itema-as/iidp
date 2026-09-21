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
	// AgePublicKey is what iidp secret set encrypts values with. The
	// matching private key exists only in the cluster. Required for
	// secret set; not required to create an Application.
	AgePublicKey string `yaml:"agePublicKey"`
	// CloudflareZone is the Cloudflare zone containing BaseDomain: the only
	// zone external-dns manages, and so the only zone in which the CLI can
	// fully automate a custom domain (--domain). Required for --domain;
	// not required to create an Application without one.
	CloudflareZone string `yaml:"cloudflareZone"`
	// BackupsBucket is the Object Storage bucket every Application database
	// is backed up to, written as platform.backupsBucket. Required for
	// --postgres; not required otherwise.
	BackupsBucket string `yaml:"backupsBucket"`
	// ObjectStorageEndpoint is the S3 endpoint of BackupsBucket's location,
	// for example https://hel1.your-objectstorage.com, written as
	// platform.objectStorageEndpoint. Required for --postgres; not required
	// otherwise.
	ObjectStorageEndpoint string `yaml:"objectStorageEndpoint"`
	// GitHubApp documents the org GitHub App iidp ci set-image authenticates
	// as: the bootstrap wizard records it here after creating and installing
	// the App, for a human reading platform.yaml to see which App and
	// installation is in play. iidp ci set-image does not read this field to
	// authenticate (see PeekGitHubAppInstallationID) and never requires it.
	GitHubApp GitHubApp `yaml:"githubApp"`
}

// GitHubApp documents the org GitHub App id and installation id the
// bootstrap wizard records after creating and installing the deploy App
// (docs/implementation-notes/05-bootstrap-wizard.md). Neither field is
// read to authenticate iidp ci set-image: the app id comes from the
// IIDP_DEPLOY_APP_ID environment variable and the installation id is
// discovered from GitHub (GET /app/installations), precisely so minting a
// credential never depends on already having one to read the Platform
// repository with (docs/implementation-notes/12-deploy-workflow.md). The
// private key is never written here either: it comes from the environment
// (internal/githubapp.PrivateKeyFromEnv).
type GitHubApp struct {
	ID             int64 `yaml:"id"`
	InstallationID int64 `yaml:"installationId"`
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

// PeekGitHubAppInstallationID does a best-effort, tolerant read of
// platform.yaml's githubApp.installationId in an already-cloned Platform
// repository at dir, for documentation and logging only: unlike
// LoadConfig, a missing file, invalid YAML, or a zero/absent id simply
// reports ok=false rather than erroring. iidp ci set-image's own operation
// never depends on this value; it is surfaced only so an operator sees
// that platform.yaml's documented installation id (if the wizard recorded
// one) agrees with reality.
func PeekGitHubAppInstallationID(dir string) (id int64, ok bool) {
	data, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if err != nil {
		return 0, false
	}
	var cfg struct {
		GitHubApp GitHubApp `yaml:"githubApp"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return 0, false
	}
	if cfg.GitHubApp.InstallationID == 0 {
		return 0, false
	}
	return cfg.GitHubApp.InstallationID, true
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
