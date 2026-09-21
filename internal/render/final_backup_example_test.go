package render_test

import (
	"fmt"

	"github.com/Itema-as/iidp/internal/render"
)

func ExampleFinalBackupCluster() {
	fmt.Println(render.FinalBackupCluster("shop"))
	fmt.Println(render.FinalBackupCluster("shop-staging"))
	// Output:
	// shop-db
	// shop-staging-db
}

func ExampleBackup() {
	out, err := render.Backup("shop-db", "2026-10-21")
	if err != nil {
		panic(err)
	}
	fmt.Print(string(out))
	// Output:
	// # The final Backup of shop-db, taken by iidp app delete and kept until 2026-10-21. The Environment's continuous WAL archive and scheduled backups already in object storage are unaffected by this object's own lifetime; see docs/implementation-notes/17-cli-add-capability-delete.md.
	// # Written by iidp; do not edit by hand.
	// apiVersion: postgresql.cnpg.io/v1
	// kind: Backup
	// metadata:
	//     name: shop-db-final
	//     annotations:
	//         iidp.itema.no/retain-until: "2026-10-21"
	// spec:
	//     cluster:
	//         name: shop-db
	//     method: plugin
	//     pluginConfiguration:
	//         name: barman-cloud.cloudnative-pg.io
}

func ExampleFinalBackupApplication() {
	out, err := render.FinalBackupApplication("shop", "prod", "shop-prod",
		"https://github.com/Itema-as/iidp-platform.git", "applications/shop/final-backup-prod")
	if err != nil {
		panic(err)
	}
	fmt.Print(string(out))
	// Output:
	// # The final Backup of shop's prod Environment, applied by iidp app delete. No resources finalizer: deleting this Application must not delete the Backup object it recorded.
	// # Written by iidp; do not edit by hand.
	// apiVersion: argoproj.io/v1alpha1
	// kind: Application
	// metadata:
	//     name: shop-final-backup-prod
	//     namespace: argocd
	//     labels:
	//         iidp.itema.no/application: shop
	//         iidp.itema.no/environment: prod
	// spec:
	//     project: default
	//     source:
	//         repoURL: https://github.com/Itema-as/iidp-platform.git
	//         targetRevision: main
	//         path: applications/shop/final-backup-prod
	//         directory:
	//             include: backup.yaml
	//     destination:
	//         server: https://kubernetes.default.svc
	//         namespace: shop-prod
	//     syncPolicy:
	//         automated:
	//             prune: false
	//             selfHeal: true
	//         syncOptions:
	//             - CreateNamespace=true
}
