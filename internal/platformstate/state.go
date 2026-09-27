// Package platformstate turns what the cluster holds for an Application
// into the live state of each of its Environments: ArgoCD's sync and
// health, the running image, the pods, the last migration and the last run
// of each Scheduled task, and the addresses. The Deploy gate's service
// answers iidp app status with it
// (docs/adr/0007-app-status-reads-through-the-deploy-gate.md,
// docs/implementation-notes/94-app-status.md), and Argus (#97) builds on
// the same types, so the two never disagree about what the cluster says.
//
// On top of what the cluster says, it interprets it (condition.go,
// deploy.go, capability.go): each Environment's Condition and Activity,
// where a Deploy is among its five hops, the Capabilities' own Condition
// and Warning flag, and the Platform components'. That layer is pure: it
// takes objects and the time now and returns domain objects
// (docs/implementation-notes/116-condition-and-activity.md).
//
// Environments are found by label, not from a fixed list: every ArgoCD
// Application in the argocd namespace labelled
// iidp.itema.no/application=<app> is one Environment, whatever its name.
// prod and staging carry the label because the CLI writes it; a Preview
// Environment appears as soon as the ArgoCD Application its ApplicationSet
// generates carries it too.
//
// The objects are read with plain list calls against the Kubernetes API
// (Kube), decoded into the few fields used here. Nothing here writes.
package platformstate

import (
	"sort"
	"strings"
	"time"
)

// The labels the lookup relies on. The CLI writes the first two on every
// Environment's ArgoCD Application and namespace, and the application chart
// puts them, and the task label, on every object it renders.
const (
	ApplicationLabel = "iidp.itema.no/application"
	EnvironmentLabel = "iidp.itema.no/environment"
	TaskLabel        = "iidp.itema.no/task"
	ComponentLabel   = "app.kubernetes.io/component"

	// ComponentMigration, ComponentScheduledTask and ComponentFinalBackup
	// are the chart's component labels for the migration Job, the
	// Scheduled task CronJobs and their Jobs, and the final backup's
	// PreDelete Job.
	ComponentMigration     = "migration"
	ComponentScheduledTask = "scheduled-task"
	ComponentFinalBackup   = "final-backup"

	// ArgoCDNamespace is where every ArgoCD Application lives.
	ArgoCDNamespace = "argocd"
)

// Status is the live state of one Application: the body of the Deploy
// gate's GET /v1/status/<app>, and what iidp app status --json prints.
// Fields are only ever added to it, never renamed or removed.
type Status struct {
	Application string `json:"application"`
	// Repository is the Application repository it is bound to, owner/name
	// as GitHub reports it now.
	Repository string `json:"repository"`
	// Environments are prod, then staging, then any others (Preview
	// Environments) by name.
	Environments []Environment `json:"environments"`
}

// Environment is the live state of one Environment.
type Environment struct {
	// Name is prod, staging, or a Preview Environment's name.
	Name string `json:"name"`
	// Namespace is the Kubernetes namespace it runs in; empty when ArgoCD
	// has no Application for it yet.
	Namespace string `json:"namespace,omitempty"`
	// ArgoCD is its ArgoCD Application's state; null when the Platform
	// repository has the Environment but ArgoCD has not picked it up yet.
	ArgoCD *ArgoCD `json:"argocd"`
	// Image is what its Deployment runs; null before the first deploy.
	Image *Image `json:"image"`
	// Pods are the Deployment's pods.
	Pods Pods `json:"pods"`
	// Migration is the last migration Job; null when none has run.
	Migration *Run `json:"migration"`
	// Tasks are its Scheduled tasks, by name.
	Tasks []Task `json:"tasks"`
	// Addresses are the URLs it answers on, from its Ingresses.
	Addresses []string `json:"addresses"`
	// Links are where to look further: ArgoCD and the logs in Grafana
	// Cloud. Absent when platform.yaml names neither.
	Links *Links `json:"links,omitempty"`
	// Condition is whether it is serving: Healthy, Degraded or Unknown,
	// judged on its own workload. Absent when ArgoCD has no Application
	// for it yet, and from a Deploy gate older than this field.
	Condition *Condition `json:"condition,omitempty"`
	// Activity is what is changing: Arriving, Unreleased, Deploying,
	// Updating or Leaving, progressing or stuck. Null when nothing is.
	Activity *Activity `json:"activity"`
}

// ArgoCD is an Environment's ArgoCD Application.
type ArgoCD struct {
	// Application is the ArgoCD Application's name.
	Application string `json:"application"`
	// Sync is Synced, OutOfSync or Unknown.
	Sync string `json:"sync"`
	// Health is Healthy, Progressing, Degraded, Suspended, Missing or
	// Unknown.
	Health string `json:"health"`
	// Operation is the last sync operation; null when there has been none.
	Operation *Operation `json:"operation"`
}

// Operation is ArgoCD's last sync operation on an Environment.
type Operation struct {
	// Phase is Running, Terminating, Succeeded, Failed or Error.
	Phase      string     `json:"phase"`
	Message    string     `json:"message,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

// Image is the image an Environment's Deployment runs.
type Image struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	// DeployedAt is when the Platform repository's commit that set this
	// tag was made. Absent when the Platform repository has no such
	// commit, as for a Preview Environment.
	DeployedAt *time.Time `json:"deployedAt,omitempty"`
}

// Pods counts the pods of an Environment's Deployment.
type Pods struct {
	Ready int `json:"ready"`
	Total int `json:"total"`
	// Restarts is the sum of their containers' restart counts.
	Restarts int `json:"restarts"`
}

// The results a Run can have.
const (
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunRunning   = "running"
)

// Run is one Job: a migration, or a Scheduled task's run.
type Run struct {
	// Result is succeeded, failed or running.
	Result     string     `json:"result"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

// Task is one Scheduled task.
type Task struct {
	Name string `json:"name"`
	// Schedule is the cron schedule, on Europe/Oslo time.
	Schedule string `json:"schedule"`
	// LastScheduleTime is when a run was last started on schedule.
	LastScheduleTime *time.Time `json:"lastScheduleTime,omitempty"`
	// LastRun is the newest of its Jobs the cluster still keeps; null
	// when there is none.
	LastRun *Run `json:"lastRun"`
}

// Links are where to look at an Environment beyond iidp app status.
type Links struct {
	ArgoCD  string `json:"argocd,omitempty"`
	Grafana string `json:"grafana,omitempty"`
}

// Objects are what the cluster holds for one Environment: its ArgoCD
// Application and, in its namespace, the Application's objects.
type Objects struct {
	ArgoCD      ArgoCDApplication
	Deployments []Deployment
	Pods        []Pod
	Jobs        []Job
	CronJobs    []CronJob
	// PostgresClusters, Certificates and Ingresses are its Capabilities'
	// objects: the CNPG Cluster, its custom domains' certificates, and
	// the Ingresses whose annotation puts it behind Itema login.
	PostgresClusters []PostgresCluster
	Certificates     []Certificate
	Ingresses        []Ingress
	// Events are Events about its ArgoCD Application: the Deploy gate's
	// DeployAccepted and DeployRefused among them. Without them a Deploy
	// is found by its tag, from Applying on.
	Events []Event
	// OutOfSyncSince is when the caller saw its ArgoCD Application turn
	// OutOfSync. A watch knows it; a List does not, and leaves it nil.
	OutOfSyncSince *time.Time
}

// EnvironmentName is the Environment an ArgoCD Application of application
// is: its iidp.itema.no/environment label, or else its namespace without
// the "<application>-" prefix, or else its own name.
func EnvironmentName(application string, app ArgoCDApplication) string {
	if name := app.Metadata.Labels[EnvironmentLabel]; name != "" {
		return name
	}
	if rest, ok := strings.CutPrefix(app.Spec.Destination.Namespace, application+"-"); ok && rest != "" {
		return rest
	}
	return app.Metadata.Name
}

// EnvironmentOf is the state of one Environment of application from its
// objects at now, its Condition and Activity included. It reads nothing
// itself.
func EnvironmentOf(application string, o Objects, now time.Time) Environment {
	env, _ := environmentOf(application, o, now)
	return env
}

func environmentOf(application string, o Objects, now time.Time) (Environment, []Deploy) {
	env := Environment{
		Name:      EnvironmentName(application, o.ArgoCD),
		Namespace: o.ArgoCD.Spec.Destination.Namespace,
		ArgoCD:    argoCDOf(o.ArgoCD),
		Tasks:     []Task{},
		Addresses: addressesOf(o.ArgoCD.Status.Summary.ExternalURLs),
	}
	if d, ok := workload(o.Deployments); ok {
		if len(d.Spec.Template.Spec.Containers) > 0 {
			repository, tag := splitImage(d.Spec.Template.Spec.Containers[0].Image)
			env.Image = &Image{Repository: repository, Tag: tag}
		}
		env.Pods = podsOf(o.Pods, d.Spec.Selector.MatchLabels)
	}

	var migrations []Job
	for _, job := range o.Jobs {
		if job.Metadata.Labels[ComponentLabel] == ComponentMigration {
			migrations = append(migrations, job)
		}
	}
	env.Migration = newestRun(migrations)

	byTask := jobsByTask(o.Jobs)
	for _, cronJob := range o.CronJobs {
		name, ok := taskName(cronJob)
		if !ok {
			continue
		}
		env.Tasks = append(env.Tasks, Task{
			Name:             name,
			Schedule:         cronJob.Spec.Schedule,
			LastScheduleTime: cronJob.Status.LastScheduleTime,
			LastRun:          newestRun(byTask[name]),
		})
	}
	sort.Slice(env.Tasks, func(i, j int) bool { return env.Tasks[i].Name < env.Tasks[j].Name })

	condition, activity, deploys := environmentState(env.Name, o, now)
	env.Condition, env.Activity = &condition, activity
	return env, deploys
}

// jobsByTask are the chart's Scheduled task Jobs by the task's name.
func jobsByTask(jobs []Job) map[string][]Job {
	out := map[string][]Job{}
	for _, job := range jobs {
		if job.Metadata.Labels[ComponentLabel] != ComponentScheduledTask {
			continue
		}
		if name := job.Metadata.Labels[TaskLabel]; name != "" {
			out[name] = append(out[name], job)
		}
	}
	return out
}

// taskName is the name of the Scheduled task a CronJob runs: its task
// label, or else its own name. It is false for a CronJob that is not a
// Scheduled task's.
func taskName(cronJob CronJob) (string, bool) {
	if cronJob.Metadata.Labels[ComponentLabel] != ComponentScheduledTask {
		return "", false
	}
	if name := cronJob.Metadata.Labels[TaskLabel]; name != "" {
		return name, true
	}
	return cronJob.Metadata.Name, true
}

// SortEnvironments orders Environments prod, staging, then the rest by
// name, with a number in a name compared as a number (pr-9 before pr-10).
func SortEnvironments(envs []Environment) {
	sort.SliceStable(envs, func(i, j int) bool { return environmentLess(envs[i].Name, envs[j].Name) })
}

// environmentLess orders Environment names as SortEnvironments does.
func environmentLess(a, b string) bool {
	rank := func(name string) int {
		switch name {
		case "prod":
			return 0
		case "staging":
			return 1
		}
		return 2
	}
	if rank(a) != rank(b) {
		return rank(a) < rank(b)
	}
	return naturalLess(a, b)
}

func naturalLess(a, b string) bool {
	pa, na := splitNumber(a)
	pb, nb := splitNumber(b)
	if pa != pb || na < 0 || nb < 0 {
		return a < b
	}
	return na < nb
}

// splitNumber splits a trailing decimal number off s: "pr-12" is "pr-", 12.
// The number is -1 when there is none.
func splitNumber(s string) (string, int) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) || len(s)-i > 9 {
		return s, -1
	}
	n := 0
	for _, c := range s[i:] {
		n = n*10 + int(c-'0')
	}
	return s[:i], n
}

func argoCDOf(app ArgoCDApplication) *ArgoCD {
	out := &ArgoCD{
		Application: app.Metadata.Name,
		Sync:        orUnknown(app.Status.Sync.Status),
		Health:      orUnknown(app.Status.Health.Status),
	}
	if op := app.Status.OperationState; op != nil && op.Phase != "" {
		out.Operation = &Operation{Phase: op.Phase, Message: op.Message, StartedAt: op.StartedAt, FinishedAt: op.FinishedAt}
	}
	return out
}

func orUnknown(s string) string {
	if s == "" {
		return "Unknown"
	}
	return s
}

// workload is the Environment's Deployment. The chart renders one; should
// there ever be more, the oldest is the Application's own.
func workload(deployments []Deployment) (Deployment, bool) {
	if len(deployments) == 0 {
		return Deployment{}, false
	}
	sorted := append([]Deployment(nil), deployments...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Metadata.CreationTimestamp.Before(sorted[j].Metadata.CreationTimestamp)
	})
	return sorted[0], true
}

// podsOf counts the pods selector matches that have not finished.
func podsOf(pods []Pod, selector map[string]string) Pods {
	var out Pods
	if len(selector) == 0 {
		return out
	}
	for _, pod := range pods {
		if !matches(pod.Metadata.Labels, selector) || pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed" {
			continue
		}
		out.Total++
		for _, c := range pod.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				out.Ready++
			}
		}
		for _, c := range pod.Status.ContainerStatuses {
			out.Restarts += c.RestartCount
		}
	}
	return out
}

func matches(labels, selector map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// newestRun is the result of the newest Job, by creation.
func newestRun(jobs []Job) *Run {
	if len(jobs) == 0 {
		return nil
	}
	newest := jobs[0]
	for _, job := range jobs[1:] {
		if job.Metadata.CreationTimestamp.After(newest.Metadata.CreationTimestamp) {
			newest = job
		}
	}
	run := &Run{Result: RunRunning, StartedAt: newest.Status.StartTime}
	for _, c := range newest.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case "Complete":
			run.Result = RunSucceeded
			run.FinishedAt = newest.Status.CompletionTime
		case "Failed":
			run.Result = RunFailed
			if c.LastTransitionTime != nil {
				run.FinishedAt = c.LastTransitionTime
			}
		}
	}
	return run
}

// addressesOf is ArgoCD's external URLs, one per host, https preferred,
// sorted.
func addressesOf(urls []string) []string {
	byHost := map[string]string{}
	for _, u := range urls {
		scheme, rest, ok := strings.Cut(u, "://")
		if !ok {
			continue
		}
		host := strings.TrimSuffix(rest, "/")
		if existing, seen := byHost[host]; seen && strings.HasPrefix(existing, "https://") {
			continue
		}
		byHost[host] = scheme + "://" + host
	}
	out := make([]string, 0, len(byHost))
	for _, u := range byHost {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// splitImage splits an image reference into its repository and its tag
// (or digest, for an image pinned by one).
func splitImage(ref string) (repository, tag string) {
	if repo, digest, ok := strings.Cut(ref, "@"); ok {
		return repo, digest
	}
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, "latest"
}
