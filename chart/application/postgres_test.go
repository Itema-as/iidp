package application_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// objectsOfKind lists the rendered "Kind/name" keys of the given Kind.
func objectsOfKind(objects map[string]object, kind string) []string {
	var found []string
	for _, key := range keys(objects) {
		if k, _, _ := strings.Cut(key, "/"); k == kind {
			found = append(found, key)
		}
	}
	return found
}

// envItem returns the container's env entry with the given name as rendered
// (value or valueFrom), or nil when there is none.
func envItem(t *testing.T, c map[string]any, name string) map[string]any {
	t.Helper()
	for _, item := range get[[]any](t, c, "env") {
		if get[string](t, item, "name") == name {
			return get[map[string]any](t, map[string]any{"item": item}, "item")
		}
	}
	return nil
}

// annotationsOf returns an object's annotations, nil when it has none.
func annotationsOf(t *testing.T, obj object) map[string]any {
	t.Helper()
	annotations, _ := get[map[string]any](t, obj, "metadata")["annotations"].(map[string]any)
	return annotations
}

func TestNothingDatabaseRelatedRendersWithPostgresOff(t *testing.T) {
	objects := render(t, "prod-small.yaml")
	for _, kind := range []string{"Cluster", "ObjectStore", "ScheduledBackup", "Job"} {
		if found := objectsOfKind(objects, kind); len(found) != 0 {
			t.Errorf("rendered %v with Postgres off", found)
		}
	}
	if item := envItem(t, container(t, objects, "shop"), "DATABASE_URL"); item != nil {
		t.Errorf("DATABASE_URL = %v with Postgres off, want none", item)
	}
}

func TestPostgresClusterIsOneInstanceNamedAfterTheEnvironment(t *testing.T) {
	cases := []struct {
		fixture, cluster string
	}{
		{"postgres-prod.yaml", "shop-db"},
		{"postgres-staging.yaml", "shop-staging-db"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			objects := render(t, tc.fixture)
			if found := objectsOfKind(objects, "Cluster"); !slices.Equal(found, []string{"Cluster/" + tc.cluster}) {
				t.Fatalf("rendered Clusters %v, want exactly Cluster/%s", found, tc.cluster)
			}
			cluster := objects["Cluster/"+tc.cluster]
			if api := get[string](t, cluster, "apiVersion"); api != "postgresql.cnpg.io/v1" {
				t.Errorf("apiVersion = %q, want postgresql.cnpg.io/v1", api)
			}
			if instances := get[int](t, cluster, "spec", "instances"); instances != 1 {
				t.Errorf("instances = %d, want 1", instances)
			}

			// Postgres is a fixed size whatever the Application's size is.
			for _, field := range []string{"requests", "limits"} {
				if cpu := get[string](t, cluster, "spec", "resources", field, "cpu"); cpu != "250m" {
					t.Errorf("resources.%s.cpu = %q, want 250m", field, cpu)
				}
				if mem := get[string](t, cluster, "spec", "resources", field, "memory"); mem != "256Mi" {
					t.Errorf("resources.%s.memory = %q, want 256Mi", field, mem)
				}
			}
			if size := get[string](t, cluster, "spec", "storage", "size"); size != "5Gi" {
				t.Errorf("storage.size = %q, want 5Gi", size)
			}

			// Both Environments get a database and an owner named after the
			// Application, so DATABASE_URL looks the same in each.
			if db := get[string](t, cluster, "spec", "bootstrap", "initdb", "database"); db != "shop" {
				t.Errorf("initdb.database = %q, want shop", db)
			}
			if owner := get[string](t, cluster, "spec", "bootstrap", "initdb", "owner"); owner != "shop" {
				t.Errorf("initdb.owner = %q, want shop", owner)
			}
		})
	}
}

func TestDatabaseURLComesFromTheClusterAppSecret(t *testing.T) {
	cases := []struct {
		fixture, deployment, secret string
	}{
		{"postgres-prod.yaml", "shop", "shop-db-app"},
		{"postgres-staging.yaml", "shop-staging", "shop-staging-db-app"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			c := container(t, render(t, tc.fixture), tc.deployment)
			item := envItem(t, c, "DATABASE_URL")
			if item == nil {
				t.Fatalf("no DATABASE_URL in env %v", get[[]any](t, c, "env"))
			}
			if _, plain := item["value"]; plain {
				t.Errorf("DATABASE_URL has a plain value; the chart must not handle credentials: %v", item)
			}
			if name := get[string](t, item, "valueFrom", "secretKeyRef", "name"); name != tc.secret {
				t.Errorf("secretKeyRef.name = %q, want %q", name, tc.secret)
			}
			if key := get[string](t, item, "valueFrom", "secretKeyRef", "key"); key != "uri" {
				t.Errorf("secretKeyRef.key = %q, want uri", key)
			}
			// PORT is still injected alongside.
			if port := envItem(t, c, "PORT"); port == nil || port["value"] != "3000" {
				t.Errorf("PORT = %v, want value 3000", port)
			}
		})
	}
}

func TestBackupsGoToTheBucketThroughTheBarmanCloudPlugin(t *testing.T) {
	const plugin = "barman-cloud.cloudnative-pg.io"
	cases := []struct {
		fixture, cluster, path, credentialsSecret, retention string
	}{
		{"postgres-prod.yaml", "shop-db", "s3://itema-iidp-db-backups/shop/prod/", "backups-credentials", "30d"},
		{"postgres-staging.yaml", "shop-staging-db", "s3://itema-iidp-db-backups/shop/staging/", "object-storage-keys", "7d"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			objects := render(t, tc.fixture)

			// The object store: where, through which endpoint, with which
			// credentials, kept for how long.
			store := mustObject(t, objects, "ObjectStore/"+tc.cluster)
			if api := get[string](t, store, "apiVersion"); api != "barmancloud.cnpg.io/v1" {
				t.Errorf("ObjectStore apiVersion = %q, want barmancloud.cnpg.io/v1", api)
			}
			if path := get[string](t, store, "spec", "configuration", "destinationPath"); path != tc.path {
				t.Errorf("destinationPath = %q, want %q", path, tc.path)
			}
			if endpoint := get[string](t, store, "spec", "configuration", "endpointURL"); endpoint != "https://hel1.your-objectstorage.com" {
				t.Errorf("endpointURL = %q, want the Object Storage endpoint", endpoint)
			}
			for ref, key := range map[string]string{"accessKeyId": "ACCESS_KEY_ID", "secretAccessKey": "ACCESS_SECRET_KEY"} {
				if name := get[string](t, store, "spec", "configuration", "s3Credentials", ref, "name"); name != tc.credentialsSecret {
					t.Errorf("s3Credentials.%s.name = %q, want %q", ref, name, tc.credentialsSecret)
				}
				if got := get[string](t, store, "spec", "configuration", "s3Credentials", ref, "key"); got != key {
					t.Errorf("s3Credentials.%s.key = %q, want %q", ref, got, key)
				}
			}
			if retention := get[string](t, store, "spec", "retentionPolicy"); retention != tc.retention {
				t.Errorf("retentionPolicy = %q, want %q", retention, tc.retention)
			}

			// The Cluster archives WAL to it through the plugin.
			plugins := get[[]any](t, mustObject(t, objects, "Cluster/"+tc.cluster), "spec", "plugins")
			if len(plugins) != 1 {
				t.Fatalf("Cluster has %d plugins, want 1", len(plugins))
			}
			if name := get[string](t, plugins[0], "name"); name != plugin {
				t.Errorf("plugin name = %q, want %s", name, plugin)
			}
			if !get[bool](t, plugins[0], "isWALArchiver") {
				t.Errorf("plugin is not the WAL archiver; there would be no continuous backup")
			}
			if store := get[string](t, plugins[0], "parameters", "barmanObjectName"); store != tc.cluster {
				t.Errorf("barmanObjectName = %q, want %q", store, tc.cluster)
			}

			// And a daily base backup through the same plugin, the first one
			// straight away so WAL has something to apply to.
			scheduled := mustObject(t, objects, "ScheduledBackup/"+tc.cluster)
			if api := get[string](t, scheduled, "apiVersion"); api != "postgresql.cnpg.io/v1" {
				t.Errorf("ScheduledBackup apiVersion = %q, want postgresql.cnpg.io/v1", api)
			}
			if cluster := get[string](t, scheduled, "spec", "cluster", "name"); cluster != tc.cluster {
				t.Errorf("ScheduledBackup cluster = %q, want %q", cluster, tc.cluster)
			}
			if schedule := get[string](t, scheduled, "spec", "schedule"); schedule != "0 0 3 * * *" {
				t.Errorf("schedule = %q, want daily at 03:00 in the six-field form", schedule)
			}
			if method := get[string](t, scheduled, "spec", "method"); method != "plugin" {
				t.Errorf("method = %q, want plugin", method)
			}
			if name := get[string](t, scheduled, "spec", "pluginConfiguration", "name"); name != plugin {
				t.Errorf("pluginConfiguration.name = %q, want %s", name, plugin)
			}
			if !get[bool](t, scheduled, "spec", "immediate") {
				t.Errorf("immediate is not true; the first base backup would wait a day")
			}
		})
	}
}

func TestMigrationJobRunsTheCommandBeforeTheRolloutOnlyWhenSet(t *testing.T) {
	t.Run("absent without a command", func(t *testing.T) {
		if found := objectsOfKind(render(t, "postgres-staging.yaml"), "Job"); len(found) != 0 {
			t.Errorf("rendered %v without a migration command", found)
		}
	})

	objects := render(t, "postgres-prod.yaml")
	job := mustObject(t, objects, "Job/shop-migrate")

	// ArgoCD runs it as a hook in a wave after the database and before the
	// Application's objects, and does not apply those if it fails: the
	// hook's failure is what stops the rollout.
	annotations := annotationsOf(t, job)
	if hook := annotations["argocd.argoproj.io/hook"]; hook != "Sync" {
		t.Errorf("hook = %v, want Sync", hook)
	}
	if policy := annotations["argocd.argoproj.io/hook-delete-policy"]; policy != "BeforeHookCreation" {
		t.Errorf("hook-delete-policy = %v, want BeforeHookCreation", policy)
	}
	waves := map[string]string{
		"Cluster/shop-db":         "-2",
		"ObjectStore/shop-db":     "-2",
		"Job/shop-migrate":        "-1",
		"ScheduledBackup/shop-db": "-1",
	}
	for key, want := range waves {
		if got := annotationsOf(t, mustObject(t, objects, key))["argocd.argoproj.io/sync-wave"]; got != want {
			t.Errorf("%s sync-wave = %v, want %q", key, got, want)
		}
	}
	// The Application's own objects stay in the default wave, after the
	// migration.
	for _, key := range []string{"Deployment/shop", "Service/shop", "Ingress/shop"} {
		if wave, set := annotationsOf(t, mustObject(t, objects, key))["argocd.argoproj.io/sync-wave"]; set {
			t.Errorf("%s sets sync-wave %v; it must stay in the default wave 0", key, wave)
		}
	}
	if limit := get[int](t, job, "spec", "backoffLimit"); limit != 0 {
		t.Errorf("backoffLimit = %d, want 0 so a failed migration fails the sync at once", limit)
	}
	if policy := get[string](t, job, "spec", "template", "spec", "restartPolicy"); policy != "Never" {
		t.Errorf("restartPolicy = %q, want Never", policy)
	}

	containers := get[[]any](t, job, "spec", "template", "spec", "containers")
	if len(containers) != 1 {
		t.Fatalf("Job has %d containers, want 1", len(containers))
	}
	c := get[map[string]any](t, map[string]any{"c": containers[0]}, "c")
	if image := get[string](t, c, "image"); image != "ghcr.io/itema-as/shop:1.4.2" {
		t.Errorf("image = %q, want the Application image", image)
	}
	command := get[[]any](t, c, "command")
	if want := []any{"sh", "-c", "npx prisma migrate deploy"}; !slices.Equal(command, want) {
		t.Errorf("command = %v, want %v", command, want)
	}
	if _, args := c["args"]; args {
		t.Errorf("Job sets args; the command is the whole shell line: %v", c["args"])
	}

	// The migration sees what the Application sees: DATABASE_URL from the
	// same Secret, the plain env, and every named Secret.
	url := envItem(t, c, "DATABASE_URL")
	if url == nil {
		t.Fatalf("no DATABASE_URL in env %v", get[[]any](t, c, "env"))
	}
	if name := get[string](t, url, "valueFrom", "secretKeyRef", "name"); name != "shop-db-app" {
		t.Errorf("secretKeyRef.name = %q, want shop-db-app", name)
	}
	if key := get[string](t, url, "valueFrom", "secretKeyRef", "key"); key != "uri" {
		t.Errorf("secretKeyRef.key = %q, want uri", key)
	}
	if nodeEnv := envItem(t, c, "NODE_ENV"); nodeEnv == nil || nodeEnv["value"] != "production" {
		t.Errorf("NODE_ENV = %v, want the Application's value", nodeEnv)
	}
	var refs []string
	for _, item := range get[[]any](t, c, "envFrom") {
		refs = append(refs, get[string](t, item, "secretRef", "name"))
	}
	if want := []string{"shop-stripe", "shop-smtp"}; !slices.Equal(refs, want) {
		t.Errorf("envFrom secretRefs = %v, want the Application's secrets %v", refs, want)
	}
}

// matches reports whether labels satisfy every key of selector.
func matches(selector, labels map[string]any) bool {
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func TestOnlyTheDeploymentPodsMatchTheServiceAndEveryPodIsAttributed(t *testing.T) {
	objects := render(t, "postgres-prod.yaml")
	selector := get[map[string]any](t, mustObject(t, objects, "Service/shop"), "spec", "selector")
	attribution := map[string]any{"iidp.itema.no/application": "shop", "iidp.itema.no/environment": "prod"}

	podLabels := map[string]map[string]any{
		"Deployment": get[map[string]any](t, mustObject(t, objects, "Deployment/shop"), "spec", "template", "metadata", "labels"),
		"Job":        get[map[string]any](t, mustObject(t, objects, "Job/shop-migrate"), "spec", "template", "metadata", "labels"),
		// CloudNativePG stamps inheritedMetadata.labels on the Pods it creates.
		"Cluster": get[map[string]any](t, mustObject(t, objects, "Cluster/shop-db"), "spec", "inheritedMetadata", "labels"),
	}
	for owner, labels := range podLabels {
		if got, want := matches(selector, labels), owner == "Deployment"; got != want {
			t.Errorf("%s Pods match the Service selector: %v, want %v (labels %v)", owner, got, want, labels)
		}
		if !matches(attribution, labels) {
			t.Errorf("%s Pods lack the Alloy attribution labels: %v", owner, labels)
		}
	}
}

func TestRenderingRefusesPostgresValuesItCannotHonour(t *testing.T) {
	cases := []struct {
		fixture, message string
	}{
		{"refuse-migration-without-postgres.yaml", `postgres.migrationCommand needs postgres.enabled: true; there is no database to migrate`},
		{"refuse-postgres-missing-bucket.yaml", `platform.backupsBucket is required when postgres.enabled`},
		{"refuse-env-sets-database-url.yaml", `env must not set DATABASE_URL; the Postgres Capability injects it`},
		{"refuse-postgres-static-site.yaml", `postgres.enabled needs kind: web-service; a Static site has no server to use a database`},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			out, err := helmTemplate(t, tc.fixture)
			if err == nil {
				t.Fatalf("rendering succeeded, want a failure:\n%s", out)
			}
			if !strings.Contains(out, tc.message) {
				t.Fatalf("error does not say %q:\n%s", tc.message, out)
			}
		})
	}
}

// crdSchemas is where kubeconform finds the CloudNativePG and Barman Cloud
// plugin CRD schemas, which the default location (built-in Kubernetes types
// only) does not carry.
const crdSchemas = "https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json"

func TestPostgresManifestsPassKubeconformWithTheCRDSchemas(t *testing.T) {
	requireTool(t, "kubeconform")
	version := kubernetesVersion(t)
	for _, fixture := range []string{"postgres-prod.yaml", "postgres-staging.yaml"} {
		t.Run(fixture, func(t *testing.T) {
			manifests, err := helmTemplate(t, fixture)
			if err != nil {
				t.Fatalf("helm template %s: %v\n%s", fixture, err, manifests)
			}
			cmd := exec.Command("kubeconform", "-strict", "-summary", "-kubernetes-version", version,
				"-schema-location", "default", "-schema-location", crdSchemas)
			cmd.Stdin = strings.NewReader(manifests)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("kubeconform: %v\n%s", err, out)
			}
			t.Logf("kubeconform: %s", strings.TrimSpace(string(out)))
		})
	}
}
