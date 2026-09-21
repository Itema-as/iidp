// Package render produces the two files that define one Environment of an
// Application in the Platform repository: the ArgoCD Application and the
// values file for the generic chart. The layout of those files is described
// in docs/platform-repository.md.
package render

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Environment is one Environment of an Application: the inputs the chart's
// values file needs.
type Environment struct {
	Application     string
	Environment     string
	BaseDomain      string
	Kind            string
	ImageRepository string
	ImageTag        string
	Size            string
	Port            int
	ProbePath       string
	// Domains are the custom domains served beside the Platform address.
	// Only prod carries any: staging keeps its Platform address (see
	// docs/implementation-notes/13-cli-capabilities.md).
	Domains []string
	// PostgresEnabled, MigrationCommand, BackupsBucket and
	// ObjectStorageEndpoint are the Postgres Capability. BackupsBucket and
	// ObjectStorageEndpoint are set only when PostgresEnabled: the chart
	// requires them only then, and they otherwise carry nothing the
	// developer chose.
	PostgresEnabled       bool
	MigrationCommand      string
	BackupsBucket         string
	ObjectStorageEndpoint string
}

// Name is the Environment's object name, <application>-<environment>: the
// ArgoCD Application's name and the Environment's namespace.
func (e Environment) Name() string {
	return e.Application + "-" + e.Environment
}

// Chart names the generic chart an ArgoCD Application installs: its OCI
// repository (without the oci:// scheme, as ArgoCD wants it), chart name
// and version.
type Chart struct {
	RepoURL string
	Name    string
	Version string
}

// ArgoCDApplication renders the ArgoCD Application for env. It installs the
// chart from the OCI registry and takes its values from valuesPath in the
// Platform repository at platformRepoURL, through ArgoCD's multi-source
// $values reference.
func ArgoCDApplication(env Environment, chart Chart, platformRepoURL, valuesPath string) ([]byte, error) {
	app := argocdApplication{
		APIVersion: "argoproj.io/v1alpha1",
		Kind:       "Application",
		Metadata: metadata{
			Name:      env.Name(),
			Namespace: "argocd",
			Labels: map[string]string{
				"iidp.itema.no/application": env.Application,
				"iidp.itema.no/environment": env.Environment,
			},
			Finalizers: []string{"resources-finalizer.argocd.argoproj.io"},
		},
		Spec: applicationSpec{
			Project: "default",
			Sources: []source{
				{
					RepoURL:        chart.RepoURL,
					Chart:          chart.Name,
					TargetRevision: chart.Version,
					Helm:           &helm{ValueFiles: []string{"$values/" + valuesPath}},
				},
				{
					RepoURL:        platformRepoURL,
					TargetRevision: "main",
					Ref:            "values",
				},
			},
			Destination: destination{
				Server:    "https://kubernetes.default.svc",
				Namespace: env.Name(),
			},
			SyncPolicy: syncPolicy{
				Automated:   automated{Prune: true, SelfHeal: true},
				SyncOptions: []string{"CreateNamespace=true"},
			},
		},
	}
	return marshal("The ArgoCD Application for the "+env.Environment+" Environment of "+env.Application+".", app)
}

// defaultBackupRetention is postgres.backupRetention's value: how long
// backups and WAL are kept in the bucket. There is no flag for it yet; it
// is written the same as the chart's own default so the file states it
// explicitly, the same reasoning as size, port and probe.path.
const defaultBackupRetention = "30d"

// Values renders the chart values file for env.
func Values(env Environment) ([]byte, error) {
	domains := env.Domains
	if domains == nil {
		domains = []string{}
	}
	v := values{
		Application: applicationValues{Name: env.Application},
		Environment: env.Environment,
		Platform: platformValues{
			BaseDomain:            env.BaseDomain,
			BackupsBucket:         env.BackupsBucket,
			ObjectStorageEndpoint: env.ObjectStorageEndpoint,
		},
		Kind:    env.Kind,
		Image:   imageValues{Repository: env.ImageRepository, Tag: env.ImageTag},
		Size:    env.Size,
		Port:    env.Port,
		Probe:   probeValues{Path: env.ProbePath},
		Env:     map[string]string{},
		Domains: domains,
		Postgres: postgresValues{
			Enabled:          env.PostgresEnabled,
			MigrationCommand: env.MigrationCommand,
			BackupRetention:  defaultBackupRetention,
		},
	}
	return marshal("Values for the "+env.Environment+" Environment of "+env.Application+". image.tag is written by the deploy workflow.", v)
}

func marshal(what string, doc any) ([]byte, error) {
	body, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	header := fmt.Sprintf("# %s\n# Written by iidp; do not edit by hand.\n", what)
	return append([]byte(header), body...), nil
}

type argocdApplication struct {
	APIVersion string          `yaml:"apiVersion"`
	Kind       string          `yaml:"kind"`
	Metadata   metadata        `yaml:"metadata"`
	Spec       applicationSpec `yaml:"spec"`
}

type metadata struct {
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace"`
	Labels    map[string]string `yaml:"labels"`
	// Finalizers is omitted (rather than rendered as an empty list) when
	// there are none: the final-backup ArgoCD Application
	// (render.FinalBackupApplication) deliberately carries no resources
	// finalizer, unlike every Environment's own Application.
	Finalizers []string `yaml:"finalizers,omitempty"`
}

type applicationSpec struct {
	Project     string      `yaml:"project"`
	Sources     []source    `yaml:"sources"`
	Destination destination `yaml:"destination"`
	SyncPolicy  syncPolicy  `yaml:"syncPolicy"`
}

type source struct {
	RepoURL        string `yaml:"repoURL"`
	Chart          string `yaml:"chart,omitempty"`
	TargetRevision string `yaml:"targetRevision"`
	Helm           *helm  `yaml:"helm,omitempty"`
	Ref            string `yaml:"ref,omitempty"`
}

type helm struct {
	ValueFiles []string `yaml:"valueFiles"`
}

type destination struct {
	Server    string `yaml:"server"`
	Namespace string `yaml:"namespace"`
}

type syncPolicy struct {
	Automated   automated `yaml:"automated"`
	SyncOptions []string  `yaml:"syncOptions"`
}

type automated struct {
	Prune    bool `yaml:"prune"`
	SelfHeal bool `yaml:"selfHeal"`
}

type values struct {
	Application applicationValues `yaml:"application"`
	Environment string            `yaml:"environment"`
	Platform    platformValues    `yaml:"platform"`
	Kind        string            `yaml:"kind"`
	Image       imageValues       `yaml:"image"`
	Size        string            `yaml:"size"`
	Port        int               `yaml:"port"`
	Probe       probeValues       `yaml:"probe"`
	Env         map[string]string `yaml:"env"`
	Domains     []string          `yaml:"domains"`
	Postgres    postgresValues    `yaml:"postgres"`
}

type applicationValues struct {
	Name string `yaml:"name"`
}

type platformValues struct {
	BaseDomain            string `yaml:"baseDomain"`
	BackupsBucket         string `yaml:"backupsBucket,omitempty"`
	ObjectStorageEndpoint string `yaml:"objectStorageEndpoint,omitempty"`
}

type postgresValues struct {
	Enabled          bool   `yaml:"enabled"`
	MigrationCommand string `yaml:"migrationCommand"`
	BackupRetention  string `yaml:"backupRetention"`
}

type imageValues struct {
	Repository string `yaml:"repository"`
	Tag        string `yaml:"tag"`
}

type probeValues struct {
	Path string `yaml:"path"`
}
