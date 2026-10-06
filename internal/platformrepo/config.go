// Package platformrepo reads and writes the Platform repository:
// platform.yaml and, per Application, one directory per Environment.
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

// Config is what the CLI reads from platform.yaml. Unknown fields are
// ignored rather than rejected.
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
	// AgePublicKey is what iidp secret set encrypts values with, and the
	// database access passwords. The matching private key exists only in
	// the cluster. Required for secret set and iidp app db access; not
	// required to create an Application, whose database then stays closed.
	AgePublicKey string `yaml:"agePublicKey"`
	// CloudflareZone is the Cloudflare zone containing BaseDomain: the only
	// zone external-dns manages, and so the only zone in which the CLI can
	// fully automate a custom domain (--domain). It is also the Itema login
	// cookie's domain (LoginCookieDomain). Not required to create an
	// Application.
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
	// GitHubApp records the org GitHub App the Deploy gate and ArgoCD
	// authenticate as, for people reading platform.yaml. Nothing reads it
	// to authenticate: the gate takes the App from a cluster Secret.
	GitHubApp GitHubApp `yaml:"githubApp"`
	// GitHubAPI is the GitHub REST API the Preview Environments' pull
	// request generator lists an Application repository's pull requests
	// with, written into each previews ApplicationSet as ArgoCD's api
	// field. Empty, the default, means GitHub.com and writes nothing; only
	// a GitHub Enterprise (https://<host>/api/v3) or a test Platform sets
	// it.
	GitHubAPI string `yaml:"githubAPI"`
}

// GitHubApp is the org GitHub App id and installation id the bootstrap
// wizard records. The private key is never written here.
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

// DeployGateHostLabel is the label under baseDomain the Deploy gate is
// served at: https://deploy.<baseDomain>, under the Platform's wildcard
// certificate. The bootstrap chart builds the gate's Ingress host the same
// way (bootstrap/templates/deploy-gate.yaml).
const DeployGateHostLabel = "deploy"

// DeployGateURL is the Deploy gate's address: where the deploy workflow
// calls it, and the audience of its GitHub Actions OIDC token. It is
// rendered into the workflow, which cannot read the private Platform
// repository.
func (c Config) DeployGateURL() string {
	return "https://" + DeployGateHostLabel + "." + c.BaseDomain
}

// DatabaseTunnelHostLabel is the label under baseDomain the database
// tunnel is served at: https://db.<baseDomain>, under the Platform's
// wildcard certificate. The bootstrap chart builds the tunnel's Ingress
// host the same way (bootstrap/components/db-tunnel).
const DatabaseTunnelHostLabel = "db"

// DatabaseTunnelURL is the database tunnel's address, where iidp app db
// connect opens its connections.
func (c Config) DatabaseTunnelURL() string {
	return "https://" + DatabaseTunnelHostLabel + "." + c.BaseDomain
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
