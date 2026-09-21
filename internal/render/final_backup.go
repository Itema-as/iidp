package render

// FinalBackupCluster is the name a final Backup targets: the CloudNativePG
// Cluster of one Environment. Callers derive it the same way the chart does
// (chart/application/templates/_helpers.tpl's application.postgres.cluster,
// docs/implementation-notes/07-chart-postgres.md): the bare Application name
// for prod, <name>-staging for staging, with -db appended.
func FinalBackupCluster(fullname string) string {
	return fullname + "-db"
}

// Backup renders the CloudNativePG Backup manifest iidp app delete commits
// for one Environment with Postgres enabled, in the plugin shape
// chart/application/templates/postgres-scheduledbackup.yaml already uses
// (method: plugin, the Barman Cloud Plugin), confirmed against the current
// CloudNativePG and Barman Cloud Plugin documentation with context7 (see
// docs/implementation-notes/17-cli-add-capability-delete.md): a Backup
// resource takes the same spec.method/spec.pluginConfiguration shape as a
// ScheduledBackup.
//
// retainUntil is a date (YYYY-MM-DD) written as the iidp.itema.no/retain-until
// annotation, 30 days ahead of the delete. Neither CloudNativePG nor the
// Barman Cloud Plugin document a per-Backup retention field or annotation
// of their own -- retention is only configured on the ObjectStore, which is
// removed along with this Backup's own Cluster -- so the annotation is a
// record for a human or a future tool to read, not something the plugin
// enforces; see the implementation notes for why the object storage data
// outlives the Backup object regardless.
func Backup(cluster, retainUntil string) ([]byte, error) {
	doc := backupManifest{
		APIVersion: "postgresql.cnpg.io/v1",
		Kind:       "Backup",
		Metadata: backupMetadata{
			Name: cluster + "-final",
			Annotations: map[string]string{
				"iidp.itema.no/retain-until": retainUntil,
			},
		},
		Spec: backupSpec{
			Cluster:             backupClusterRef{Name: cluster},
			Method:              "plugin",
			PluginConfiguration: pluginConfigurationRef{Name: "barman-cloud.cloudnative-pg.io"},
		},
	}
	return marshal("The final Backup of "+cluster+", taken by iidp app delete and kept until "+retainUntil+". The Environment's continuous WAL archive and scheduled backups already in object storage are unaffected by this object's own lifetime; see docs/implementation-notes/17-cli-add-capability-delete.md.", doc)
}

// FinalBackupApplication renders the ArgoCD Application that applies one
// Environment's final Backup manifest: a plain directory source on the
// Platform repository itself, restricted to backup.yaml, with no resources
// finalizer (deleting this Application later must not delete the Backup
// object it recorded) and pruning off (nothing here should ever be removed
// by a sync). path is applications/<name>/final-backup-<environment>, the
// directory backup.yaml lives in; its own application.yaml one level up in
// the same directory matches bootstrap/applications.yaml's include glob
// (*/*/application.yaml, docs/implementation-notes/09-e2e-fixture-application.md)
// exactly like every other Environment's Application.
func FinalBackupApplication(application, environment, namespace, platformRepoURL, path string) ([]byte, error) {
	app := finalBackupApplication{
		APIVersion: "argoproj.io/v1alpha1",
		Kind:       "Application",
		Metadata: metadata{
			Name:      application + "-final-backup-" + environment,
			Namespace: "argocd",
			Labels: map[string]string{
				"iidp.itema.no/application": application,
				"iidp.itema.no/environment": environment,
			},
		},
		Spec: finalBackupApplicationSpec{
			Project: "default",
			Source: finalBackupSource{
				RepoURL:        platformRepoURL,
				TargetRevision: "main",
				Path:           path,
				Directory:      finalBackupDirectory{Include: "backup.yaml"},
			},
			Destination: destination{
				Server:    "https://kubernetes.default.svc",
				Namespace: namespace,
			},
			SyncPolicy: finalBackupSyncPolicy{
				Automated:   finalBackupAutomated{Prune: false, SelfHeal: true},
				SyncOptions: []string{"CreateNamespace=true"},
			},
		},
	}
	return marshal("The final Backup of "+application+"'s "+environment+" Environment, applied by iidp app delete. No resources finalizer: deleting this Application must not delete the Backup object it recorded.", app)
}

type backupManifest struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   backupMetadata `yaml:"metadata"`
	Spec       backupSpec     `yaml:"spec"`
}

type backupMetadata struct {
	Name        string            `yaml:"name"`
	Annotations map[string]string `yaml:"annotations"`
}

type backupSpec struct {
	Cluster             backupClusterRef       `yaml:"cluster"`
	Method              string                 `yaml:"method"`
	PluginConfiguration pluginConfigurationRef `yaml:"pluginConfiguration"`
}

type backupClusterRef struct {
	Name string `yaml:"name"`
}

type pluginConfigurationRef struct {
	Name string `yaml:"name"`
}

type finalBackupApplication struct {
	APIVersion string                     `yaml:"apiVersion"`
	Kind       string                     `yaml:"kind"`
	Metadata   metadata                   `yaml:"metadata"`
	Spec       finalBackupApplicationSpec `yaml:"spec"`
}

type finalBackupApplicationSpec struct {
	Project     string                `yaml:"project"`
	Source      finalBackupSource     `yaml:"source"`
	Destination destination           `yaml:"destination"`
	SyncPolicy  finalBackupSyncPolicy `yaml:"syncPolicy"`
}

type finalBackupSource struct {
	RepoURL        string               `yaml:"repoURL"`
	TargetRevision string               `yaml:"targetRevision"`
	Path           string               `yaml:"path"`
	Directory      finalBackupDirectory `yaml:"directory"`
}

type finalBackupDirectory struct {
	Include string `yaml:"include"`
}

type finalBackupSyncPolicy struct {
	Automated   finalBackupAutomated `yaml:"automated"`
	SyncOptions []string             `yaml:"syncOptions"`
}

type finalBackupAutomated struct {
	Prune    bool `yaml:"prune"`
	SelfHeal bool `yaml:"selfHeal"`
}
