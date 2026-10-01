package render

import (
	"errors"
	"fmt"
	"path"

	"gopkg.in/yaml.v3"
)

// Preview Environments: one ArgoCD ApplicationSet per Application that opted
// in, whose Pull Request generator makes an Environment for each open pull
// request labelled PreviewLabel, and removes it when the pull request
// closes or loses the label.

const (
	// PreviewLabel is the pull request label that asks for a Preview
	// Environment. The reusable deploy workflow builds the pull request's
	// image only when it is there.
	PreviewLabel = "preview"

	// PreviewRequeueSeconds is how often ArgoCD asks GitHub for the
	// Application repository's pull requests: a trivial share of the App's
	// API rate limit.
	PreviewRequeueSeconds = 180

	// GitHubAppSecret is the Secret in the argocd namespace that holds the
	// iidp-deploy GitHub App's credential, which cloud-init writes for
	// ArgoCD. The Pull Request generator authenticates to GitHub with it.
	GitHubAppSecret = "platform-repo-github-app"

	// PreviewSize is a Preview Environment's size, whatever staging's.
	PreviewSize = "small"

	// PreviewRetryLimit is how many times ArgoCD retries a Preview
	// Environment's failed sync. ArgoCD retries inside the same operation,
	// and deletes an Application only once it has no operation running, so
	// with no limit a preview whose sync keeps failing is never removed when
	// its pull request closes. A new push starts a fresh sync anyway.
	PreviewRetryLimit = 3
)

// PreviewEnvironment is the Environment name of the Preview Environment
// for pull request number, which may be the template parameter
// "{{.number}}".
func PreviewEnvironment(number string) string {
	return "pr-" + number
}

// Previews is what an Application's ApplicationSet is rendered from.
type Previews struct {
	Application string
	// Owner and Repository name the Application repository on GitHub.
	Owner, Repository string
	// GitHubAPI is the GitHub REST API the generator asks, as ArgoCD's api
	// field takes it for GitHub Enterprise. Empty means GitHub.com.
	GitHubAPI string
	// Staging is the staging Environment's application.yaml. A preview
	// copies its sources, so it gets staging's chart version, values file
	// and secrets.
	Staging []byte
}

// previewValues is what a preview sets over staging's values file, through
// helm.valuesObject, which ArgoCD gives precedence over valueFiles.
type previewValues struct {
	Environment string          `yaml:"environment"`
	Image       previewImage    `yaml:"image"`
	Size        string          `yaml:"size"`
	Domains     []string        `yaml:"domains"`
	RunTasks    bool            `yaml:"runTasks"`
	Login       previewLogin    `yaml:"login"`
	Postgres    previewPostgres `yaml:"postgres"`
}

type previewImage struct {
	Tag string `yaml:"tag"`
}

type previewLogin struct {
	Enabled bool `yaml:"enabled"`
}

type previewPostgres struct {
	Backups bool `yaml:"backups"`
}

// appSource is one source of an ArgoCD Application as a preview copies it
// from staging's.
type appSource struct {
	RepoURL        string   `yaml:"repoURL"`
	Chart          string   `yaml:"chart,omitempty"`
	Path           string   `yaml:"path,omitempty"`
	TargetRevision string   `yaml:"targetRevision"`
	Helm           *appHelm `yaml:"helm,omitempty"`
	Ref            string   `yaml:"ref,omitempty"`
}

type appHelm struct {
	ValueFiles   []string       `yaml:"valueFiles"`
	ValuesObject *previewValues `yaml:"valuesObject,omitempty"`
}

type applicationSet struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	Metadata   metadata           `yaml:"metadata"`
	Spec       applicationSetSpec `yaml:"spec"`
}

type applicationSetSpec struct {
	GoTemplate        bool           `yaml:"goTemplate"`
	GoTemplateOptions []string       `yaml:"goTemplateOptions"`
	Generators        []generator    `yaml:"generators"`
	Template          appSetTemplate `yaml:"template"`
}

type generator struct {
	PullRequest pullRequestGenerator `yaml:"pullRequest"`
}

type pullRequestGenerator struct {
	GitHub              gitHubPullRequests `yaml:"github"`
	RequeueAfterSeconds int                `yaml:"requeueAfterSeconds"`
}

type gitHubPullRequests struct {
	Owner         string   `yaml:"owner"`
	Repo          string   `yaml:"repo"`
	API           string   `yaml:"api,omitempty"`
	AppSecretName string   `yaml:"appSecretName"`
	Labels        []string `yaml:"labels"`
}

type appSetTemplate struct {
	Metadata metadata       `yaml:"metadata"`
	Spec     previewAppSpec `yaml:"spec"`
}

type previewAppSpec struct {
	Project     string      `yaml:"project"`
	Sources     []appSource `yaml:"sources"`
	Destination destination `yaml:"destination"`
	SyncPolicy  syncPolicy  `yaml:"syncPolicy"`
}

// PreviewApplicationSet renders the ApplicationSet that gives p's
// Application a Preview Environment for every open pull request labelled
// PreviewLabel. The template is staging's ArgoCD Application, with
// previewValues over staging's values file. The tracking annotation on the
// namespace makes ArgoCD delete it with the Application: otherwise ArgoCD
// leaves a namespace CreateNamespace made behind.
func PreviewApplicationSet(p Previews) ([]byte, error) {
	if p.Application == "" || p.Owner == "" || p.Repository == "" {
		return nil, errors.New("previews need the Application and its repository's owner and name")
	}
	sources, err := previewSources(p.Application, p.Staging)
	if err != nil {
		return nil, err
	}

	environment := PreviewEnvironment("{{.number}}")
	name := p.Application + "-" + environment
	values := &previewValues{
		Environment: environment,
		Image:       previewImage{Tag: "{{.head_sha}}"},
		Size:        PreviewSize,
		Domains:     []string{},
		RunTasks:    false,
		Login:       previewLogin{Enabled: true},
		Postgres:    previewPostgres{Backups: false},
	}
	for i := range sources {
		if sources[i].Helm != nil {
			sources[i].Helm.ValuesObject = values
		}
	}

	set := applicationSet{
		APIVersion: "argoproj.io/v1alpha1",
		Kind:       "ApplicationSet",
		Metadata: metadata{
			Name:      p.Application + "-previews",
			Namespace: "argocd",
			Labels:    map[string]string{ApplicationNamespaceLabel: p.Application},
		},
		Spec: applicationSetSpec{
			GoTemplate: true,
			// A parameter the template names but the generator does not
			// give fails the ApplicationSet instead of rendering "<no value>".
			GoTemplateOptions: []string{"missingkey=error"},
			Generators: []generator{{PullRequest: pullRequestGenerator{
				GitHub: gitHubPullRequests{
					Owner:         p.Owner,
					Repo:          p.Repository,
					API:           p.GitHubAPI,
					AppSecretName: GitHubAppSecret,
					Labels:        []string{PreviewLabel},
				},
				RequeueAfterSeconds: PreviewRequeueSeconds,
			}}},
			Template: appSetTemplate{
				Metadata: metadata{
					Name:      name,
					Namespace: "argocd",
					Labels: map[string]string{
						ApplicationNamespaceLabel:   p.Application,
						"iidp.itema.no/environment": environment,
					},
					Finalizers: []string{"resources-finalizer.argocd.argoproj.io"},
				},
				Spec: previewAppSpec{
					Project:     "default",
					Sources:     sources,
					Destination: destination{Server: "https://kubernetes.default.svc", Namespace: name},
					SyncPolicy: syncPolicy{
						Automated:   automated{Prune: true, SelfHeal: true},
						SyncOptions: []string{"CreateNamespace=true"},
						ManagedNamespaceMetadata: &namespaceMetadata{
							Labels: NamespaceLabels(p.Application, environment),
							Annotations: map[string]string{
								"argocd.argoproj.io/tracking-id": name + ":/Namespace:/" + name,
							},
						},
						Retry: retry{
							Limit:   PreviewRetryLimit,
							Refresh: true,
							Backoff: backoff{Duration: "10s", Factor: 2, MaxDuration: "3m"},
						},
					},
				},
			},
		},
	}
	return marshal("The Preview Environments of "+p.Application+": one Environment for each open pull request labelled "+PreviewLabel+" on "+p.Owner+"/"+p.Repository+", removed with its namespace and database when the pull request closes or loses the label.", set)
}

// previewSources reads the sources of staging's application.yaml and checks
// they are the shape the CLI writes: one chart source whose values file is
// staging's, and the Platform repository under ref values.
func previewSources(application string, stagingApplication []byte) ([]appSource, error) {
	var staging struct {
		Spec struct {
			Sources []appSource `yaml:"sources"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(stagingApplication, &staging); err != nil {
		return nil, fmt.Errorf("parsing staging's application.yaml: %w", err)
	}
	wantValues := "$values/" + path.Join("applications", application, "staging", "values.yaml")
	charts, refs := 0, 0
	for _, s := range staging.Spec.Sources {
		switch {
		case s.Helm != nil:
			charts++
			if len(s.Helm.ValueFiles) != 1 || s.Helm.ValueFiles[0] != wantValues {
				return nil, fmt.Errorf("staging's application.yaml gives the chart the values files %v, want only %s", s.Helm.ValueFiles, wantValues)
			}
		case s.Ref == "values":
			refs++
		}
	}
	if charts != 1 || refs != 1 {
		return nil, fmt.Errorf("staging's application.yaml has %d chart sources and %d values sources, want one of each", charts, refs)
	}
	return staging.Spec.Sources, nil
}
