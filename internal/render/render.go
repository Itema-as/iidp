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
	// RunAsNonRoot says the image runs as a non-root user with a numeric
	// UID, as the built-in templates' images do: the chart then requires
	// it (runAsNonRoot), drops every capability, and serves a Static site
	// on 8080. Left false for an image iidp did not generate, which may
	// run as root (docs/implementation-notes/90-guardrails.md).
	RunAsNonRoot bool
	// Domains are the custom domains served beside the Platform address.
	// Only prod carries any: staging keeps its Platform address (see
	// docs/implementation-notes/13-cli-capabilities.md).
	Domains []string
	// PostgresEnabled, MigrationCommand, BackupsBucket and
	// ObjectStorageEndpoint are the Postgres Capability. BackupsBucket and
	// ObjectStorageEndpoint are set only when PostgresEnabled: the chart
	// requires them only then, and they otherwise carry nothing the
	// developer chose. The CLI leaves MigrationCommand empty: the Deploy
	// gate writes it with each deploy, from the Application repository's
	// iidp.yaml (docs/implementation-notes/66-migration-command-in-repo.md).
	PostgresEnabled       bool
	MigrationCommand      string
	BackupsBucket         string
	ObjectStorageEndpoint string
	// Login is the Itema login Capability: written the same in every
	// Environment, since one oauth2-proxy cookie covers both
	// (docs/implementation-notes/18-itema-login.md).
	Login bool
	// LoginCookieDomain is platform.loginCookieDomain, the domain the login
	// cookie is set for: platform.yaml's cloudflareZone, or BaseDomain
	// without one. Set only with Login, the one Capability the chart needs
	// it for (docs/implementation-notes/76-login-in-zone-domains.md).
	LoginCookieDomain string
	// LoginGroups are the sign-in groups, Entra ID group object ids: with
	// any, only their members get past Itema login. Written the same in
	// every Environment, and always, empty included, like domains
	// (docs/implementation-notes/92-sign-in-groups.md).
	LoginGroups []string
}

// Name is the Environment's object name, <application>-<environment>: the
// ArgoCD Application's name and the Environment's namespace.
func (e Environment) Name() string {
	return e.Application + "-" + e.Environment
}

// ApplicationNamespaceLabel is the Namespace label that marks an
// Application namespace. Every Environment's namespace carries it, with
// the Environment's Application as its value, and the bootstrap's
// guardrails bind their admission policies to namespaces that have it
// (bootstrap/components/guardrails). Platform namespaces never carry it.
const ApplicationNamespaceLabel = "iidp.itema.no/application"

// NamespaceLabels are the labels ArgoCD sets on an Environment's namespace
// (syncPolicy.managedNamespaceMetadata): the Environment's identity, the
// same two labels its ArgoCD Application and every chart object carry,
// and Pod Security Admission's levels. baseline is enforced, so no Pod
// runs privileged, shares the host's namespaces or mounts its paths;
// restricted is warned about and audited, and an image built from a
// template meets it (docs/implementation-notes/90-guardrails.md).
func NamespaceLabels(application, environment string) map[string]string {
	return map[string]string{
		ApplicationNamespaceLabel:            application,
		"iidp.itema.no/environment":          environment,
		"pod-security.kubernetes.io/enforce": "baseline",
		"pod-security.kubernetes.io/warn":    "restricted",
		"pod-security.kubernetes.io/audit":   "restricted",
	}
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
				// ArgoCD applies these to the namespace CreateNamespace
				// creates, and to the existing one on every sync. They are
				// what the guardrails select Application namespaces by.
				ManagedNamespaceMetadata: &namespaceMetadata{
					Labels: NamespaceLabels(env.Application, env.Environment),
				},
				// The same policy as the Platform's own Applications
				// (bootstrap/templates/_helpers.tpl): retry until it works,
				// each time against the newest commit. Without refresh, a
				// failing sync (a migration, say) keeps retrying the commit
				// it started on while the fix already sits in the Platform
				// repository (#75).
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
		Kind:  env.Kind,
		Image: imageValues{Repository: env.ImageRepository, Tag: env.ImageTag},
		Size:  env.Size,
		Port:  env.Port,
		Probe: probeValues{Path: env.ProbePath},
		// Written only when true: an Environment written before #90, or
		// for an image iidp did not generate, reads the same as the
		// chart's default, and a staging copied from prod keeps it.
		RunAsNonRoot: env.RunAsNonRoot,
		Env:          map[string]string{},
		Domains:      domains,
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
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace"`
	Labels    map[string]string `yaml:"labels"`
	// Finalizers is omitted (rather than rendered as an empty list) when
	// there are none. Every Environment's Application carries the
	// resources finalizer; nothing in this package renders one without it
	// today, but the field stays optional rather than hard-coded.
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
	Automated                automated          `yaml:"automated"`
	SyncOptions              []string           `yaml:"syncOptions"`
	ManagedNamespaceMetadata *namespaceMetadata `yaml:"managedNamespaceMetadata,omitempty"`
	Retry                    retry              `yaml:"retry"`
}

type namespaceMetadata struct {
	Labels map[string]string `yaml:"labels"`
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
	// RunAsNonRoot is omitted when false (see Values).
	RunAsNonRoot bool              `yaml:"runAsNonRoot,omitempty"`
	Env          map[string]string `yaml:"env"`
	Domains      []string          `yaml:"domains"`
	Postgres     postgresValues    `yaml:"postgres"`
	Login        loginValues       `yaml:"login"`
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

// loginValues is the Itema login Capability: one shared oauth2-proxy in
// front of every Ingress of the Environment. Written in full, even when
// disabled, the same convention postgres and domains already follow
// (docs/implementation-notes/13-cli-capabilities.md).
type loginValues struct {
	Enabled bool     `yaml:"enabled"`
	Groups  []string `yaml:"groups"`
}
