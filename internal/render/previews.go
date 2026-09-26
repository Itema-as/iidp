package render

import (
	"errors"
	"fmt"
	"path"

	"gopkg.in/yaml.v3"
)

// Preview Environments (docs/adr/0006-preview-environments-from-an-argocd-applicationset.md):
// one ArgoCD ApplicationSet per Application that opted in, whose Pull
// Request generator makes an Environment for each open pull request
// labelled PreviewLabel on the Application repository, and removes it when
// the pull request closes or loses the label.

const (
	// PreviewLabel is the pull request label that asks for a Preview
	// Environment. The reusable deploy workflow builds the pull request's
	// image only when it is there (.github/workflows/application-deploy.yaml).
	PreviewLabel = "preview"

	// PreviewRequeueSeconds is how often ArgoCD asks GitHub for the
	// Application repository's pull requests: every 3 minutes, a trivial
	// share of the App's API rate limit (ADR-0006).
	PreviewRequeueSeconds = 180

	// GitHubAppSecret is the Secret in the argocd namespace that holds the
	// iidp-deploy GitHub App's id, installation id and private key: the one
	// cloud-init writes as ArgoCD's credential for the Platform repository
	// (docs/implementation-notes/41-argocd-platform-repo-credential.md). The
	// Pull Request generator authenticates to GitHub with it.
	GitHubAppSecret = "platform-repo-github-app"

	// PreviewSize is a Preview Environment's size, whatever staging's: the
	// smallest.
	PreviewSize = "small"
)

// PreviewEnvironment is the Environment name of the Preview Environment
// for pull request number, as the ApplicationSet's template spells it for
// the generator's number parameter ("{{.number}}") or as it is for one
// pull request ("7" gives pr-7).
func PreviewEnvironment(number string) string {
	return "pr-" + number
}

// Previews is what an Application's ApplicationSet is rendered from.
type Previews struct {
	// Application is the Application's name.
	Application string
	// Owner and Repository name the Application repository on GitHub, as
	// the Platform knows it (applications/<name>/repository.yaml).
	Owner, Repository string
	// GitHubAPI is the GitHub REST API the generator asks, as ArgoCD's api
	// field takes it for GitHub Enterprise (https://<host>/api/v3). Empty
	// means GitHub.com.
	GitHubAPI string
	// Staging is the staging Environment's application.yaml. A preview
	// renders staging's chart source at its pinned version, with staging's
	// values file as the base, and every other source staging has: the
	// Platform repository for $values and, once staging has secrets,
	// staging's sops/ directory, so a preview gets staging's secrets
	// through the same KSOPS path.
	Staging []byte
}

// previewValues is what a preview sets over staging's values file
// (helm.valuesObject, which ArgoCD gives precedence over valueFiles): its
// own Environment name and so its own address, the pull request's image,
// the smallest size, Itema login on with staging's sign-in groups, no
// custom domains, no Scheduled tasks, and a database without backups.
// Staging's migration command, env, secrets and the rest stay as they are.
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
// from staging's: the chart (from an OCI repository with chart, or a git
// repository with path), the Platform repository under ref values, or a
// kustomize directory with path.
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
// PreviewLabel. The template is staging's ArgoCD Application with, for pull
// request <n>:
//
//   - the name, namespace and Environment <application>-pr-<n>, pr-<n>, so
//     the chart gives it the address <application>-pr-<n>.<baseDomain>;
//   - staging's sources, with previewValues over staging's values file on
//     the chart source;
//   - the namespace labels every Environment's namespace carries, so the
//     guardrails bind to it (NamespaceLabels), and ArgoCD's tracking
//     annotation on it, which makes ArgoCD delete the namespace with the
//     Application when the pull request closes. ArgoCD leaves a namespace
//     CreateNamespace made behind otherwise; this is the way its sync
//     options documentation gives to have it owned.
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
							Limit:   -1,
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
// staging's, and the Platform repository under ref values. A kustomize
// source (staging's sops/) is kept as it is.
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
