package render_test

import (
	"fmt"

	"github.com/Itema-as/iidp/internal/render"
)

var shop = render.Environment{
	Application:     "shop",
	Environment:     "prod",
	BaseDomain:      "app.itma.no",
	Kind:            "web-service",
	ImageRepository: "ghcr.io/itema-as/shop",
	Size:            "small",
	Port:            3000,
	ProbePath:       "/",
}

func ExampleArgoCDApplication() {
	chart := render.Chart{RepoURL: "ghcr.io/itema-as/charts", Name: "application", Version: "0.3.1"}
	out, err := render.ArgoCDApplication(shop, chart, "https://github.com/Itema-as/iidp-platform.git", "applications/shop/prod/values.yaml")
	if err != nil {
		panic(err)
	}
	fmt.Print(string(out))
	// Output:
	// # The ArgoCD Application for the prod Environment of shop.
	// # Written by iidp; do not edit by hand.
	// apiVersion: argoproj.io/v1alpha1
	// kind: Application
	// metadata:
	//     name: shop-prod
	//     namespace: argocd
	//     labels:
	//         iidp.itema.no/application: shop
	//         iidp.itema.no/environment: prod
	//     finalizers:
	//         - resources-finalizer.argocd.argoproj.io
	// spec:
	//     project: default
	//     sources:
	//         - repoURL: ghcr.io/itema-as/charts
	//           chart: application
	//           targetRevision: 0.3.1
	//           helm:
	//             valueFiles:
	//                 - $values/applications/shop/prod/values.yaml
	//         - repoURL: https://github.com/Itema-as/iidp-platform.git
	//           targetRevision: main
	//           ref: values
	//     destination:
	//         server: https://kubernetes.default.svc
	//         namespace: shop-prod
	//     syncPolicy:
	//         automated:
	//             prune: true
	//             selfHeal: true
	//         syncOptions:
	//             - CreateNamespace=true
}

func ExampleValues() {
	out, err := render.Values(shop)
	if err != nil {
		panic(err)
	}
	fmt.Print(string(out))
	// Output:
	// # Values for the prod Environment of shop. image.tag is written by the deploy workflow.
	// # Written by iidp; do not edit by hand.
	// application:
	//     name: shop
	// environment: prod
	// platform:
	//     baseDomain: app.itma.no
	// kind: web-service
	// image:
	//     repository: ghcr.io/itema-as/shop
	//     tag: ""
	// size: small
	// port: 3000
	// probe:
	//     path: /
	// env: {}
	// domains: []
	// postgres:
	//     enabled: false
	//     migrationCommand: ""
	//     backupRetention: 30d
}
