package render_test

import (
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/render"
)

// stagingApplication is shop's staging application.yaml as the CLI writes
// it, with the sops/ source iidp secret set adds once staging has a secret.
func stagingApplication(withSecrets bool) []byte {
	staging := shop
	staging.Environment = "staging"
	chart := render.Chart{RepoURL: "ghcr.io/itema-as/charts", Name: "application", Version: "0.3.1"}
	out, err := render.ArgoCDApplication(staging, chart, "https://github.com/Itema-as/iidp-platform.git", "applications/shop/staging/values.yaml")
	if err != nil {
		panic(err)
	}
	if withSecrets {
		out, _, err = render.AddKustomizeSource(out, "https://github.com/Itema-as/iidp-platform.git", "applications/shop/staging/sops")
		if err != nil {
			panic(err)
		}
	}
	return out
}

func ExamplePreviewApplicationSet() {
	out, err := render.PreviewApplicationSet(render.Previews{
		Application: "shop",
		Owner:       "Itema-as",
		Repository:  "shop",
		Staging:     stagingApplication(true),
	})
	if err != nil {
		panic(err)
	}
	fmt.Print(string(out))
	// Output:
	// # The Preview Environments of shop: one Environment for each open pull request labelled preview on Itema-as/shop, removed with its namespace and database when the pull request closes or loses the label.
	// # Written by iidp; do not edit by hand.
	// apiVersion: argoproj.io/v1alpha1
	// kind: ApplicationSet
	// metadata:
	//     name: shop-previews
	//     namespace: argocd
	//     labels:
	//         iidp.itema.no/application: shop
	// spec:
	//     goTemplate: true
	//     goTemplateOptions:
	//         - missingkey=error
	//     generators:
	//         - pullRequest:
	//             github:
	//                 owner: Itema-as
	//                 repo: shop
	//                 appSecretName: platform-repo-github-app
	//                 labels:
	//                     - preview
	//             requeueAfterSeconds: 180
	//     template:
	//         metadata:
	//             name: shop-pr-{{.number}}
	//             namespace: argocd
	//             labels:
	//                 iidp.itema.no/application: shop
	//                 iidp.itema.no/environment: pr-{{.number}}
	//             finalizers:
	//                 - resources-finalizer.argocd.argoproj.io
	//         spec:
	//             project: default
	//             sources:
	//                 - repoURL: ghcr.io/itema-as/charts
	//                   chart: application
	//                   targetRevision: 0.3.1
	//                   helm:
	//                     valueFiles:
	//                         - $values/applications/shop/staging/values.yaml
	//                     valuesObject:
	//                         environment: pr-{{.number}}
	//                         image:
	//                             tag: '{{.head_sha}}'
	//                         size: small
	//                         domains: []
	//                         runTasks: false
	//                         login:
	//                             enabled: true
	//                         postgres:
	//                             backups: false
	//                 - repoURL: https://github.com/Itema-as/iidp-platform.git
	//                   targetRevision: main
	//                   ref: values
	//                 - repoURL: https://github.com/Itema-as/iidp-platform.git
	//                   path: applications/shop/staging/sops
	//                   targetRevision: main
	//             destination:
	//                 server: https://kubernetes.default.svc
	//                 namespace: shop-pr-{{.number}}
	//             syncPolicy:
	//                 automated:
	//                     prune: true
	//                     selfHeal: true
	//                 syncOptions:
	//                     - CreateNamespace=true
	//                 managedNamespaceMetadata:
	//                     labels:
	//                         iidp.itema.no/application: shop
	//                         iidp.itema.no/environment: pr-{{.number}}
	//                         pod-security.kubernetes.io/audit: restricted
	//                         pod-security.kubernetes.io/enforce: baseline
	//                         pod-security.kubernetes.io/warn: restricted
	//                     annotations:
	//                         argocd.argoproj.io/tracking-id: shop-pr-{{.number}}:/Namespace:/shop-pr-{{.number}}
	//                 retry:
	//                     limit: 3
	//                     refresh: true
	//                     backoff:
	//                         duration: 10s
	//                         factor: 2
	//                         maxDuration: 3m
}

// The generator's api is written only for a GitHub other than GitHub.com.
func TestPreviewApplicationSetNamesTheGitHubAPIOnlyWhenGiven(t *testing.T) {
	for _, api := range []string{"", "https://github.example.com/api/v3"} {
		out, err := render.PreviewApplicationSet(render.Previews{Application: "shop", Owner: "Itema-as", Repository: "shop", GitHubAPI: api, Staging: stagingApplication(false)})
		if err != nil {
			t.Fatal(err)
		}
		var set struct {
			Spec struct {
				Generators []struct {
					PullRequest struct {
						GitHub map[string]any `yaml:"github"`
					} `yaml:"pullRequest"`
				} `yaml:"generators"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(out, &set); err != nil {
			t.Fatal(err)
		}
		got, ok := set.Spec.Generators[0].PullRequest.GitHub["api"]
		if api == "" && ok {
			t.Errorf("api = %v, want none for GitHub.com", got)
		}
		if api != "" && got != api {
			t.Errorf("api = %v, want %s", got, api)
		}
	}
}

// A preview retries a failed sync a limited number of times, so a sync that
// keeps failing ends and lets ArgoCD delete the preview when its pull
// request closes (#131). The staging Environment it copies still retries
// without limit (#75).
func TestPreviewRetriesALimitedNumberOfTimesWhereStagingRetriesForEver(t *testing.T) {
	type syncRetry struct {
		Limit   int  `yaml:"limit"`
		Refresh bool `yaml:"refresh"`
	}
	staging := stagingApplication(false)
	var app struct {
		Spec struct {
			SyncPolicy struct {
				Retry syncRetry `yaml:"retry"`
			} `yaml:"syncPolicy"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(staging, &app); err != nil {
		t.Fatal(err)
	}
	if got := app.Spec.SyncPolicy.Retry; got.Limit != -1 || !got.Refresh {
		t.Errorf("staging's retry = %+v, want limit -1 with refresh", got)
	}

	out, err := render.PreviewApplicationSet(render.Previews{Application: "shop", Owner: "Itema-as", Repository: "shop", Staging: staging})
	if err != nil {
		t.Fatal(err)
	}
	var set struct {
		Spec struct {
			Template struct {
				Spec struct {
					SyncPolicy struct {
						Retry syncRetry `yaml:"retry"`
					} `yaml:"syncPolicy"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(out, &set); err != nil {
		t.Fatal(err)
	}
	got := set.Spec.Template.Spec.SyncPolicy.Retry
	if got.Limit < 0 || got.Limit != render.PreviewRetryLimit {
		t.Errorf("preview's retry limit = %d, want %d, a finite number", got.Limit, render.PreviewRetryLimit)
	}
	if !got.Refresh {
		t.Error("preview's retry has no refresh; each retry must sync the newest commit")
	}
}

// A staging application.yaml the CLI did not write is refused rather than
// guessed at.
func TestPreviewApplicationSetRefusesAStagingItCannotReadTheChartFrom(t *testing.T) {
	cases := map[string]string{
		"not YAML": "spec: [",
		"no chart source": `spec:
  sources:
    - repoURL: https://github.com/Itema-as/iidp-platform.git
      targetRevision: main
      ref: values
`,
		"another values file": `spec:
  sources:
    - repoURL: ghcr.io/itema-as/charts
      chart: application
      targetRevision: 0.3.1
      helm:
        valueFiles:
          - $values/applications/shop/prod/values.yaml
    - repoURL: https://github.com/Itema-as/iidp-platform.git
      targetRevision: main
      ref: values
`,
		"no values source": `spec:
  sources:
    - repoURL: ghcr.io/itema-as/charts
      chart: application
      targetRevision: 0.3.1
      helm:
        valueFiles:
          - $values/applications/shop/staging/values.yaml
`,
	}
	for name, staging := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := render.PreviewApplicationSet(render.Previews{Application: "shop", Owner: "Itema-as", Repository: "shop", Staging: []byte(staging)}); err == nil {
				t.Error("rendered, want a refusal")
			}
		})
	}
	if _, err := render.PreviewApplicationSet(render.Previews{Application: "shop", Staging: stagingApplication(false)}); err == nil || !strings.Contains(err.Error(), "repository") {
		t.Errorf("without a repository: err = %v, want a refusal naming it", err)
	}
}
