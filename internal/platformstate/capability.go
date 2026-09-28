package platformstate

import (
	"sort"
	"strings"
	"time"
)

// The Capabilities judged here, by what the cluster holds for them.
const (
	CapabilityPostgres      = "postgres"
	CapabilityItemaLogin    = "itema-login"
	CapabilityCustomDomain  = "custom-domain"
	CapabilityScheduledTask = "scheduled-task"
)

// The Itema login Capability is only an annotation on the Environment's
// Ingresses (chart/application/templates/_helpers.tpl,
// application.login.annotation): Traefik's middlewares annotation names
// the Platform's shared ForwardAuth middleware, or, with sign-in groups,
// the Environment's own copy of it, <namespace>-<fullname>-itema-login.
const (
	MiddlewaresAnnotation = "traefik.ingress.kubernetes.io/router.middlewares"
	sharedLoginMiddleware = "oauth2-proxy-itema-login-auth@kubernetescrd"
	ownLoginMiddleware    = "-itema-login@kubernetescrd"
)

// cnpgSettingUpPrimary is CNPG's phase for a Cluster being created.
const cnpgSettingUpPrimary = "Setting up primary"

// Capability is one Capability of an Environment. Its Condition is its
// own and does not roll up into the Environment's, which judges only its
// own workload.
type Capability struct {
	// Type is postgres, itema-login, custom-domain or scheduled-task.
	Type string `json:"type"`
	// Name is the CNPG Cluster's name, itema-login, the custom domain's
	// host, or the Scheduled task's name.
	Name      string    `json:"name"`
	Condition Condition `json:"condition"`
	// Activity is Arriving while a database or a certificate is first
	// being set up, and Leaving while it is deleted; null otherwise.
	// Adding or removing a Capability is its Environment's Updating.
	Activity *Activity `json:"activity"`
}

// CapabilitiesOf are the Capabilities of one Environment from its
// objects: its Postgres, its Itema login (by its Ingresses), its custom
// domains (by their certificates) and its Scheduled tasks, in that order.
func CapabilitiesOf(o Objects, now time.Time) []Capability {
	out := []Capability{}
	for _, c := range o.PostgresClusters {
		out = append(out, postgresCapability(c, now))
	}
	if c, ok := loginCapability(o.Ingresses); ok {
		out = append(out, c)
	}
	for _, c := range o.Certificates {
		out = append(out, domainCapability(c, now))
	}
	var tasks []Capability
	byTask := jobsByTask(o.Jobs)
	for _, cronJob := range o.CronJobs {
		if name, ok := taskName(cronJob); ok {
			tasks = append(tasks, taskCapability(name, cronJob, byTask[name]))
		}
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Name < tasks[j].Name })
	return append(out, tasks...)
}

// loginCapability is the Itema login Capability, when an Ingress of the
// Environment is behind it. It has no state of its own to judge: the
// ForwardAuth service it relies on is a Platform component, oauth2-proxy.
// It is Leaving once every such Ingress is being deleted.
func loginCapability(ingresses []Ingress) (Capability, bool) {
	found, allLeaving := false, true
	for _, ing := range ingresses {
		if !behindItemaLogin(ing) {
			continue
		}
		found = true
		allLeaving = allLeaving && ing.Metadata.DeletionTimestamp != nil
	}
	if !found {
		return Capability{}, false
	}
	c := Capability{Type: CapabilityItemaLogin, Name: CapabilityItemaLogin, Condition: Condition{State: Healthy}}
	if allLeaving {
		c.Activity = &Activity{State: Leaving}
	}
	return c, true
}

// behindItemaLogin reports whether an Ingress's middlewares annotation
// names the shared Itema login middleware or an Environment's own copy.
func behindItemaLogin(ing Ingress) bool {
	for _, m := range strings.Split(ing.Metadata.Annotations[MiddlewaresAnnotation], ",") {
		m = strings.TrimSpace(m)
		if m == sharedLoginMiddleware || (strings.HasSuffix(m, ownLoginMiddleware) && len(m) > len(ownLoginMiddleware)) {
			return true
		}
	}
	return false
}

// taskCapability is a Scheduled task. A failed last run (its CronJob's
// newest Job, the one Tasks shows) is a Warning, as failing backups are:
// the Environment serves all the same, so it is never Degraded, and the
// Environment's own Condition and Activity do not change.
func taskCapability(name string, cronJob CronJob, jobs []Job) Capability {
	c := Capability{Type: CapabilityScheduledTask, Name: name, Condition: Condition{State: Healthy}, Activity: leaving(cronJob.Metadata)}
	if run := newestRun(jobs); run != nil && run.Result == RunFailed {
		c.Condition.Warning = "the last run failed"
		if run.FinishedAt != nil {
			c.Condition.Warning += " at " + run.FinishedAt.UTC().Format("2006-01-02 15:04 UTC")
		}
	}
	return c
}

func leaving(m ObjectMeta) *Activity {
	if m.DeletionTimestamp != nil {
		return &Activity{State: Leaving}
	}
	return nil
}

// readyCondition judges an object by its Ready condition: Healthy when
// True, Unknown when Unknown, and Degraded once it has been anything else
// for longer than NotReadyGrace.
func readyCondition(conditions []KubeCondition, created, now time.Time, fallback string) Condition {
	ready, ok := conditionOf(conditions, "Ready")
	switch {
	case ok && ready.Status == conditionTrue:
		return Condition{State: Healthy}
	case ok && ready.Status == conditionUnknown:
		return Condition{State: Unknown, Reason: orElse(ready.Message, fallback)}
	}
	since := created
	if ok && ready.LastTransitionTime != nil {
		since = *ready.LastTransitionTime
	}
	if now.Sub(since) > NotReadyGrace {
		return Condition{State: Degraded, Reason: orElse(ready.Message, fallback)}
	}
	return Condition{State: Healthy}
}

func orElse(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

func postgresCapability(c PostgresCluster, now time.Time) Capability {
	out := Capability{Type: CapabilityPostgres, Name: c.Metadata.Name, Activity: leaving(c.Metadata)}
	if c.Status.Phase == cnpgSettingUpPrimary {
		out.Condition = Condition{State: Healthy}
		if out.Activity == nil {
			out.Activity = &Activity{State: Arriving}
		}
	} else {
		out.Condition = readyCondition(c.Status.Conditions, c.Metadata.CreationTimestamp, now, orElse(c.Status.Phase, "the database is not ready"))
	}
	if b, ok := conditionOf(c.Status.Conditions, "LastBackupSucceeded"); ok && b.Status == conditionFalse {
		out.Condition.Warning = "backups are failing: " + orElse(b.Message, b.Reason)
	} else if a, ok := conditionOf(c.Status.Conditions, "ContinuousArchiving"); ok && a.Status == conditionFalse {
		out.Condition.Warning = "WAL archiving is failing: " + orElse(a.Message, a.Reason)
	}
	return out
}

func domainCapability(c Certificate, now time.Time) Capability {
	name := c.Metadata.Name
	if len(c.Spec.DNSNames) > 0 {
		name = c.Spec.DNSNames[0]
	}
	out := Capability{Type: CapabilityCustomDomain, Name: name, Activity: leaving(c.Metadata)}
	if c.Status.NotAfter == nil {
		// Never issued yet: the domain is arriving, and stuck once an
		// issuance has failed.
		out.Condition = Condition{State: Healthy}
		if out.Activity == nil {
			out.Activity = &Activity{State: Arriving}
			if c.Status.LastFailureTime != nil {
				ready, _ := conditionOf(c.Status.Conditions, "Ready")
				out.Activity.Stuck, out.Activity.Reason = true, "issuing the certificate failed: "+orElse(ready.Message, "see cert-manager's events")
			}
		}
		return out
	}
	out.Condition = readyCondition(c.Status.Conditions, c.Metadata.CreationTimestamp, now, "the certificate is not ready")
	if c.Status.NotAfter.Sub(now) < CertificateWarningWithin {
		out.Condition.Warning = "the certificate expires " + c.Status.NotAfter.UTC().Format("2006-01-02 15:04 UTC")
	}
	return out
}

// ComponentObjects are what the cluster holds for one Platform component.
type ComponentObjects struct {
	// Name is the component's, such as argocd or traefik.
	Name string
	// ArgoCD is the ArgoCD Application that manages it; nil for Traefik
	// and k3s's own parts, which ArgoCD does not manage.
	ArgoCD      *ArgoCDApplication
	Deployments []Deployment
	// Pods are its pods, those of its Deployments and any others (a
	// StatefulSet's, a DaemonSet's).
	Pods []Pod
	// Nodes are the nodes it is, for k3s itself: each is judged by its
	// Ready condition.
	Nodes []Node
	// OutOfSyncSince is when the caller saw its ArgoCD Application turn
	// OutOfSync; nil when it does not know.
	OutOfSyncSince *time.Time
}

// Component is one Platform component: a Condition, plus Updating for one
// ArgoCD manages. It never Arrives or Leaves: it is part of the Platform.
type Component struct {
	Name      string    `json:"name"`
	Condition Condition `json:"condition"`
	Activity  *Activity `json:"activity"`
}

// ComponentOf is the state of one Platform component from its objects.
func ComponentOf(o ComponentObjects, now time.Time) Component {
	out := Component{Name: o.Name, Condition: Condition{State: Healthy}}
	var all []facts
	owned := map[string]bool{}
	for _, d := range o.Deployments {
		f := facts{app: o.ArgoCD, workload: &d, desired: desiredReplicas(d), pods: runningPods(o.Pods, d.Spec.Selector.MatchLabels), outOfSyncSince: o.OutOfSyncSince, now: now}
		for _, pod := range f.pods {
			owned[pod.Metadata.Namespace+"/"+pod.Metadata.Name] = true
		}
		all = append(all, f)
		out.Condition = worse(out.Condition, workloadCondition(f))
	}
	for _, pod := range o.Pods {
		if owned[pod.Metadata.Namespace+"/"+pod.Metadata.Name] || pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed" {
			continue
		}
		out.Condition = worse(out.Condition, podsCondition([]Pod{pod}, 1, now, true, now))
	}
	for _, node := range o.Nodes {
		c := readyCondition(node.Status.Conditions, node.Metadata.CreationTimestamp, now, "the node is not ready")
		if c.Reason != "" {
			c.Reason = "node " + node.Metadata.Name + ": " + c.Reason
		}
		out.Condition = worse(out.Condition, c)
	}
	if o.ArgoCD == nil {
		return out
	}
	switch health := o.ArgoCD.Status.Health.Status; {
	case out.Condition.State == Healthy && health == Unknown:
		out.Condition = Condition{State: Unknown, Reason: "ArgoCD reports its health as Unknown"}
	case len(o.Deployments) == 0 && len(o.Pods) == 0 && len(o.Nodes) == 0 && health == Degraded:
		// None of its workload is in view (Argus watches only argocd's
		// and kube-system's), so ArgoCD's app-level health is all there
		// is to judge it by.
		out.Condition = Condition{State: Degraded, Reason: "ArgoCD reports its health as Degraded"}
	}

	f := facts{app: o.ArgoCD, outOfSyncSince: o.OutOfSyncSince, now: now}
	stuck := syncStuck(o.ArgoCD)
	updating := changing(f)
	for _, wf := range all {
		updating = updating || changing(wf)
		if stuck == "" {
			stuck = rolloutStuck(wf, "")
		}
	}
	if stuck == "" {
		stuck = outOfSyncStuck(f, outOfSyncFrom(f))
	}
	if updating || stuck != "" {
		out.Activity = &Activity{State: Updating, Stuck: stuck != "", Reason: stuck}
	}
	return out
}

// worse is the worse of two Conditions: Degraded, then Unknown, then
// Healthy. The first wins a tie.
func worse(a, b Condition) Condition {
	rank := map[string]int{Healthy: 0, Unknown: 1, Degraded: 2}
	if rank[b.State] > rank[a.State] {
		return b
	}
	return a
}

// Application is one Application with everything Argus shows for it: each
// Environment's state, its Capabilities and its Deploys.
type Application struct {
	Name         string             `json:"name"`
	Environments []EnvironmentState `json:"environments"`
}

// EnvironmentState is an Environment with its Capabilities and its
// Deploys, oldest first.
type EnvironmentState struct {
	Environment
	Capabilities []Capability `json:"capabilities"`
	Deploys      []Deploy     `json:"deploys"`
}

// ApplicationOf is an Application from the objects of each of its
// Environments, in SortEnvironments order.
func ApplicationOf(name string, envs []Objects, now time.Time) Application {
	out := Application{Name: name, Environments: []EnvironmentState{}}
	for _, o := range envs {
		env, deploys := environmentOf(name, o, now)
		if deploys == nil {
			deploys = []Deploy{}
		}
		out.Environments = append(out.Environments, EnvironmentState{Environment: env, Capabilities: CapabilitiesOf(o, now), Deploys: deploys})
	}
	sort.SliceStable(out.Environments, func(i, j int) bool {
		return environmentLess(out.Environments[i].Name, out.Environments[j].Name)
	})
	return out
}
