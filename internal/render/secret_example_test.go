package render_test

import (
	"fmt"

	"github.com/Itema-as/iidp/internal/render"
)

func ExampleSecretDocument() {
	out, err := render.SecretDocument(render.Secret{
		Name:        "shop-api-key",
		Application: "shop",
		Environment: "prod",
		Key:         "API_KEY",
		Value:       "hunter2",
	})
	if err != nil {
		panic(err)
	}
	fmt.Print(string(out))
	// Output:
	// apiVersion: v1
	// kind: Secret
	// metadata:
	//     name: shop-api-key
	//     labels:
	//         app.kubernetes.io/name: shop
	//         iidp.itema.no/application: shop
	//         iidp.itema.no/environment: prod
	//     annotations:
	//         argocd.argoproj.io/sync-wave: "-2"
	//         kustomize.config.k8s.io/needs-hash: "false"
	// type: Opaque
	// stringData:
	//     API_KEY: hunter2
}

func ExampleSopsKustomization() {
	fmt.Print(string(render.SopsKustomization()))
	// Output:
	// apiVersion: kustomize.config.k8s.io/v1beta1
	// kind: Kustomization
	// generators:
	//   - ksops.yaml
}

func ExampleKsopsGenerator() {
	out := render.KsopsGenerator("shop-secrets", []string{"db-password.enc.yaml", "api-key.enc.yaml"})
	fmt.Print(string(out))
	// Output:
	// apiVersion: viaduct.ai/v1
	// kind: ksops
	// metadata:
	//     name: shop-secrets
	//     annotations:
	//         config.kubernetes.io/function: |
	//             exec:
	//               path: ksops
	// files:
	//     - api-key.enc.yaml
	//     - db-password.enc.yaml
}
