package argus

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Itema-as/iidp/internal/platformstate"
)

// How the model finds the Platform's components among the ArgoCD
// Applications in argocd, and their workloads in argocd and kube-system.
// ArgoCD 3.x tracks what it manages with an annotation,
// <application>:<group>/<kind>:<namespace>/<name>
// (util/argo/resource_tracking.go); with label tracking it is the
// instance label instead.
const (
	TrackingAnnotation = "argocd.argoproj.io/tracking-id"
	instanceLabel      = "app.kubernetes.io/instance"
	partOfLabel        = "app.kubernetes.io/part-of"
	// componentsOwner is the ArgoCD Application that renders the
	// bootstrap: every Application it manages is a Platform component.
	componentsOwner = "platform-components"
	// traefikServiceLabel marks k3s's ServiceLB pods for Traefik's
	// LoadBalancer Service.
	traefikServiceLabel = "svccontroller.k3s.cattle.io/svcname"

	// The components that are not ArgoCD's: Traefik and k3s itself (the
	// node, CoreDNS, metrics-server, the local-path provisioner and the
	// rest of kube-system), which k3s installs.
	ComponentTraefik = "traefik"
	ComponentK3s     = "k3s"

	kubeSystem = "kube-system"
)

// objects are the cut-down objects the informers keep, gathered by kind.
type objects struct {
	argoCD       []platformstate.ArgoCDApplication
	deployments  []platformstate.Deployment
	pods         []platformstate.Pod
	jobs         []platformstate.Job
	cronJobs     []platformstate.CronJob
	clusters     []platformstate.PostgresCluster
	certificates []platformstate.Certificate
	ingresses    []platformstate.Ingress
	events       []platformstate.Event
	nodes        []platformstate.Node
}

// gather sorts what the store holds by kind. Each list is in a stable
// order (namespace, then name), so the model built from it is too. An
// object two informers both hold (their selectors overlap) counts once.
func gather(all map[string]map[string]any) objects {
	var keys []string
	flat := map[string]any{}
	for _, byKey := range all {
		for key, obj := range byKey {
			k := key + "\x00" + fmt.Sprintf("%T", obj)
			if _, dup := flat[k]; !dup {
				keys = append(keys, k)
			}
			flat[k] = obj
		}
	}
	sort.Strings(keys)
	var o objects
	for _, k := range keys {
		switch v := flat[k].(type) {
		case platformstate.ArgoCDApplication:
			o.argoCD = append(o.argoCD, v)
		case platformstate.Deployment:
			o.deployments = append(o.deployments, v)
		case platformstate.Pod:
			o.pods = append(o.pods, v)
		case platformstate.Job:
			o.jobs = append(o.jobs, v)
		case platformstate.CronJob:
			o.cronJobs = append(o.cronJobs, v)
		case platformstate.PostgresCluster:
			o.clusters = append(o.clusters, v)
		case platformstate.Certificate:
			o.certificates = append(o.certificates, v)
		case platformstate.Ingress:
			o.ingresses = append(o.ingresses, v)
		case platformstate.Event:
			o.events = append(o.events, v)
		case platformstate.Node:
			o.nodes = append(o.nodes, v)
		}
	}
	return o
}

// build interprets the objects at now: every Application with its
// Environments, and every Platform component, each by name. Each
// Environment gets its links from platform, and its image's deployedAt
// (platform.go), which build keeps in known.
func build(o objects, outOfSyncSince map[string]time.Time, platform Platform, known map[string]time.Time, now time.Time) (map[string]platformstate.Application, map[string]platformstate.Component) {
	byNamespace := map[string]*platformstate.Objects{}
	in := func(ns string) *platformstate.Objects {
		if byNamespace[ns] == nil {
			byNamespace[ns] = &platformstate.Objects{}
		}
		return byNamespace[ns]
	}
	for _, d := range o.deployments {
		in(d.Metadata.Namespace).Deployments = append(in(d.Metadata.Namespace).Deployments, d)
	}
	for _, p := range o.pods {
		in(p.Metadata.Namespace).Pods = append(in(p.Metadata.Namespace).Pods, p)
	}
	for _, j := range o.jobs {
		in(j.Metadata.Namespace).Jobs = append(in(j.Metadata.Namespace).Jobs, j)
	}
	for _, c := range o.cronJobs {
		in(c.Metadata.Namespace).CronJobs = append(in(c.Metadata.Namespace).CronJobs, c)
	}
	for _, c := range o.clusters {
		in(c.Metadata.Namespace).PostgresClusters = append(in(c.Metadata.Namespace).PostgresClusters, c)
	}
	for _, c := range o.certificates {
		in(c.Metadata.Namespace).Certificates = append(in(c.Metadata.Namespace).Certificates, c)
	}
	for _, i := range o.ingresses {
		in(i.Metadata.Namespace).Ingresses = append(in(i.Metadata.Namespace).Ingresses, i)
	}
	var gate []platformstate.Event
	for _, e := range o.events {
		if e.Reason == platformstate.ReasonDeployAccepted || e.Reason == platformstate.ReasonDeployRefused {
			gate = append(gate, e)
		}
	}

	envs := map[string][]platformstate.Objects{}
	componentApps := map[string]*platformstate.ArgoCDApplication{}
	for i := range o.argoCD {
		app := o.argoCD[i]
		name := app.Metadata.Labels[platformstate.ApplicationLabel]
		if name == "" {
			if isComponent(app) {
				componentApps[app.Metadata.Name] = &o.argoCD[i]
			}
			continue
		}
		// An Environment's objects are what its namespace holds: the
		// CLI gives every Environment a namespace of its own.
		env := platformstate.Objects{ArgoCD: app}
		if ns := app.Spec.Destination.Namespace; ns != "" && byNamespace[ns] != nil && ns != platformstate.ArgoCDNamespace && ns != kubeSystem {
			n := byNamespace[ns]
			env.Deployments, env.Pods, env.Jobs, env.CronJobs = n.Deployments, n.Pods, n.Jobs, n.CronJobs
			env.PostgresClusters, env.Certificates, env.Ingresses = n.PostgresClusters, n.Certificates, n.Ingresses
		}
		envName := platformstate.EnvironmentName(name, app)
		for _, e := range gate {
			a := e.Metadata.Annotations
			if e.Regarding.Name == app.Metadata.Name || (a[platformstate.AnnotationApplication] == name && a[platformstate.AnnotationEnvironment] == envName) {
				env.Events = append(env.Events, e)
			}
		}
		if since, ok := outOfSyncSince[app.Metadata.Namespace+"/"+app.Metadata.Name]; ok {
			env.OutOfSyncSince = &since
		}
		envs[name] = append(envs[name], env)
	}
	apps := map[string]platformstate.Application{}
	live := map[string]bool{}
	for name, objs := range envs {
		app := withLinks(platformstate.ApplicationOf(name, objs, now), platform)
		for i := range app.Environments {
			env := &app.Environments[i]
			if env.Image == nil || env.ArgoCD == nil {
				continue
			}
			for _, o := range objs {
				if o.ArgoCD.Metadata.Name == env.ArgoCD.Application {
					env.Image.DeployedAt = deployedAt(*env, o.ArgoCD, known)
					live[o.ArgoCD.Metadata.Name+"@"+env.Image.Tag] = true
				}
			}
		}
		apps[name] = app
	}
	for key := range known {
		if !live[key] {
			delete(known, key)
		}
	}

	components := map[string]platformstate.Component{}
	for name, c := range componentObjects(o, componentApps, byNamespace) {
		if c.ArgoCD != nil {
			if since, ok := outOfSyncSince[c.ArgoCD.Metadata.Namespace+"/"+c.ArgoCD.Metadata.Name]; ok {
				c.OutOfSyncSince = &since
			}
		}
		components[name] = platformstate.ComponentOf(c, now)
	}
	return apps, components
}

// isComponent reports whether an ArgoCD Application without the
// iidp.itema.no/application label is one of the bootstrap's components:
// platform-components manages it.
func isComponent(app platformstate.ArgoCDApplication) bool {
	return trackedBy(app.Metadata) == componentsOwner
}

// trackedBy is the ArgoCD Application that manages an object, from its
// tracking annotation or, failing that, its instance label.
func trackedBy(m platformstate.ObjectMeta) string {
	if id := m.Annotations[TrackingAnnotation]; id != "" {
		owner, _, _ := strings.Cut(id, ":")
		return owner
	}
	return m.Labels[instanceLabel]
}

// componentObjects gathers each Platform component's objects: the
// component ArgoCD Applications with the Deployments in argocd and
// kube-system they manage and those Deployments' pods; Traefik; and k3s,
// which is the node and whatever else runs in kube-system.
func componentObjects(o objects, apps map[string]*platformstate.ArgoCDApplication, byNamespace map[string]*platformstate.Objects) map[string]platformstate.ComponentObjects {
	out := map[string]platformstate.ComponentObjects{}
	for name, app := range apps {
		out[name] = platformstate.ComponentObjects{Name: name, ArgoCD: app}
	}
	add := func(name string, d *platformstate.Deployment, p *platformstate.Pod) {
		c, ok := out[name]
		if !ok {
			c = platformstate.ComponentObjects{Name: name}
		}
		if d != nil {
			c.Deployments = append(c.Deployments, *d)
		}
		if p != nil {
			c.Pods = append(c.Pods, *p)
		}
		out[name] = c
	}

	for _, ns := range []string{platformstate.ArgoCDNamespace, kubeSystem} {
		n := byNamespace[ns]
		if n == nil {
			continue
		}
		owner := map[int]string{} // Deployment index -> component
		for i := range n.Deployments {
			d := &n.Deployments[i]
			name := trackedBy(d.Metadata)
			switch {
			case apps[name] != nil:
			case ns == kubeSystem && d.Metadata.Name == ComponentTraefik:
				name = ComponentTraefik
			case ns == kubeSystem:
				name = ComponentK3s
			case d.Metadata.Labels[partOfLabel] == "argocd" && apps["argocd"] != nil:
				name = "argocd"
			default:
				continue
			}
			owner[i] = name
			add(name, d, nil)
		}
		for i := range n.Pods {
			p := &n.Pods[i]
			name := ""
			for j, d := range n.Deployments {
				if c, ok := owner[j]; ok && selects(d.Spec.Selector.MatchLabels, p.Metadata.Labels) {
					name = c
					break
				}
			}
			if name == "" {
				switch {
				case ns == kubeSystem && p.Metadata.Labels[traefikServiceLabel] == ComponentTraefik:
					name = ComponentTraefik
				case ns == kubeSystem:
					name = ComponentK3s
				case p.Metadata.Labels[partOfLabel] == "argocd" && apps["argocd"] != nil:
					// The application controller's StatefulSet.
					name = "argocd"
				default:
					continue
				}
			}
			add(name, nil, p)
		}
	}
	if len(o.nodes) > 0 {
		c := out[ComponentK3s]
		c.Name = ComponentK3s
		c.Nodes = o.nodes
		out[ComponentK3s] = c
	}
	return out
}

// selects reports whether a Deployment's selector matches a pod's labels.
func selects(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// placeOf is where an Event happened: the Environment its object is in or
// is about, a component's ArgoCD Application, or else the Platform.
func placeOf(e platformstate.Event, argoCD []platformstate.ArgoCDApplication) Place {
	a := e.Metadata.Annotations
	if app, env := a[platformstate.AnnotationApplication], a[platformstate.AnnotationEnvironment]; app != "" && env != "" {
		return Place{Application: app, Environment: env}
	}
	ns := e.Regarding.Namespace
	if ns == "" {
		ns = e.Metadata.Namespace
	}
	for _, app := range argoCD {
		name := app.Metadata.Labels[platformstate.ApplicationLabel]
		switch {
		case e.Regarding.Kind == "Application" && ns == app.Metadata.Namespace && e.Regarding.Name == app.Metadata.Name && name != "":
			return Place{Application: name, Environment: platformstate.EnvironmentName(name, app)}
		case e.Regarding.Kind == "Application" && ns == app.Metadata.Namespace && e.Regarding.Name == app.Metadata.Name && isComponent(app):
			return Place{Component: app.Metadata.Name}
		case name != "" && e.Regarding.Kind != "Application" && ns != "" && ns == app.Spec.Destination.Namespace:
			return Place{Application: name, Environment: platformstate.EnvironmentName(name, app)}
		}
	}
	if ns == kubeSystem || e.Regarding.Kind == "Node" {
		return Place{Component: ComponentK3s}
	}
	return Place{}
}
