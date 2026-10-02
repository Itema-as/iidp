// Package render produces and edits the files that define an Application's
// Environments in the Platform repository: the ArgoCD Application and the
// values file for the generic chart.
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
	// Only prod carries any.
	Domains []string
	// PostgresEnabled, MigrationCommand, BackupsBucket and
	// ObjectStorageEndpoint are the Postgres Capability. BackupsBucket and
	// ObjectStorageEndpoint are set only when PostgresEnabled. The CLI leaves
	// MigrationCommand empty: the Deploy gate writes it from iidp.yaml.
	PostgresEnabled       bool
	MigrationCommand      string
	BackupsBucket         string
	ObjectStorageEndpoint string
	// Login is the Itema login Capability: the same in every Environment,
	// since one oauth2-proxy login cookie covers all of them.
	Login bool
	// LoginCookieDomain is the domain the login cookie is set for:
	// platform.yaml's cloudflareZone, or BaseDomain without one. Set only
	// with Login.
	LoginCookieDomain string
	// LoginGroups are the sign-in groups, Entra ID group object ids: with
	// any, only their members get past Itema login.
	LoginGroups []string
}

// Name is the Environment's object name, <application>-<environment>: the
// ArgoCD Application's name and the Environment's namespace.
func (e Environment) Name() string {
	return e.Application + "-" + e.Environment
}

// ApplicationNamespaceLabel marks an Application namespace, with the
// Application as its value. The bootstrap's guardrails bind their admission
// policies to namespaces that have it; Platform namespaces never carry it.
const ApplicationNamespaceLabel = "iidp.itema.no/application"

// NamespaceLabels are the labels ArgoCD sets on an Environment's namespace:
// the Environment's identity and Pod Security Admission's levels. warn and
// audit also apply to the workloads that make Pods, so a Deployment whose
// Pods would be refused says so when ArgoCD applies it.
func NamespaceLabels(application, environment string) map[string]string {
	return map[string]string{
		ApplicationNamespaceLabel:            application,
		"iidp.itema.no/environment":          environment,
		"pod-security.kubernetes.io/enforce": "restricted",
		"pod-security.kubernetes.io/warn":    "restricted",
		"pod-security.kubernetes.io/audit":   "restricted",
	}
}

// Chart names the generic chart an ArgoCD Application installs. RepoURL
// has no oci:// scheme, as ArgoCD wants it.
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
				ManagedNamespaceMetadata: &namespaceMetadata{
					Labels: NamespaceLabels(env.Application, env.Environment),
				},
				// Retry until it works, each time against the newest commit.
				// Without refresh, a failing sync (a migration, say) keeps
				// retrying the commit it started on while the fix already
				// sits in the Platform repository.
				Retry: retry{
					Limit:   -1,
					Refresh: true,
					Backoff: backoff{Duration: "10s", Factor: 2, MaxDuration: "3m"},
				},
			},
		},
	}
	return marshal("The ArgoCD Application for the "+env.Environment+" Environment of "+env.Application+".", app)
}

// defaultBackupRetention is the chart's own default, written so the values
// file states it explicitly.
const defaultBackupRetention = "30d"

// Values renders the chart values file for env.
func Values(env Environment) ([]byte, error) {
	domains := env.Domains
	if domains == nil {
		domains = []string{}
	}
	loginGroups := env.LoginGroups
	if loginGroups == nil {
		loginGroups = []string{}
	}
	v := values{
		Application: applicationValues{Name: env.Application},
		Environment: env.Environment,
		Platform: platformValues{
			BaseDomain:            env.BaseDomain,
			LoginCookieDomain:     env.LoginCookieDomain,
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
		Login: loginValues{Enabled: env.Login, Groups: loginGroups},
	}
	return marshal("Values for the "+env.Environment+" Environment of "+env.Application+". image.tag is written by the deploy workflow; until then it is empty and the Environment renders nothing.", v)
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
	Name       string            `yaml:"name"`
	Namespace  string            `yaml:"namespace"`
	Labels     map[string]string `yaml:"labels"`
	Finalizers []string          `yaml:"finalizers,omitempty"`
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
	Automated                automated          `yaml:"automated"`
	SyncOptions              []string           `yaml:"syncOptions"`
	ManagedNamespaceMetadata *namespaceMetadata `yaml:"managedNamespaceMetadata,omitempty"`
	Retry                    retry              `yaml:"retry"`
}

type namespaceMetadata struct {
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

type retry struct {
	Limit   int     `yaml:"limit"`
	Refresh bool    `yaml:"refresh"`
	Backoff backoff `yaml:"backoff"`
}

type backoff struct {
	Duration    string `yaml:"duration"`
	Factor      int    `yaml:"factor"`
	MaxDuration string `yaml:"maxDuration"`
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
	Login       loginValues       `yaml:"login"`
}

type applicationValues struct {
	Name string `yaml:"name"`
}

type platformValues struct {
	BaseDomain            string `yaml:"baseDomain"`
	LoginCookieDomain     string `yaml:"loginCookieDomain,omitempty"`
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

// loginValues is the Itema login Capability, written in full even when
// disabled, like postgres and domains.
type loginValues struct {
	Enabled bool     `yaml:"enabled"`
	Groups  []string `yaml:"groups"`
}
