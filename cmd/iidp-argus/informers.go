package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"

	"github.com/Itema-as/iidp/internal/argus"
	"github.com/Itema-as/iidp/internal/platformstate"
)

// A source is one informer: a resource, narrowed to a namespace, a label
// selector or a field selector, and the transform that keeps only what
// platformstate reads of each object. The cache never holds the unstructured
// object, which is what keeps Argus within its memory budget.
type source struct {
	name      string
	gvr       schema.GroupVersionResource
	namespace string
	labels    string
	fields    string
	keep      func(*unstructured.Unstructured) (any, error)
}

// Resources Argus reads, and nothing else: its ClusterRole grants get,
// list and watch on exactly these (bootstrap/components/argus).
var (
	applicationsGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	deploymentsGVR  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	podsGVR         = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	jobsGVR         = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
	cronJobsGVR     = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}
	eventsGVR       = schema.GroupVersionResource{Version: "v1", Resource: "events"}
	ingressesGVR    = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}
	clustersGVR     = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}
	certificatesGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
	nodesGVR        = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
)

// Where the Platform components run that Argus judges by their pods.
const (
	argocdNamespace     = platformstate.ArgoCDNamespace
	kubeSystemNamespace = "kube-system"
	// applicationSelector is every object the application chart renders,
	// in every Environment's namespace: they all carry the label.
	applicationSelector = platformstate.ApplicationLabel
)

// sources are Argus's informers. Certificates are watched everywhere:
// cert-manager's ingress-shim makes them from the chart's Ingress, so Argus
// does not rely on them carrying its labels, and they are few. Events are
// every one in argocd (the Deploy gate's carry no labels) and Warnings
// elsewhere. They are read through core v1, whose field selector knows type;
// events.k8s.io/v1 serves the same objects.
var sources = []source{
	{name: "applications", gvr: applicationsGVR, namespace: argocdNamespace, keep: keepArgoCD},
	{name: "deployments", gvr: deploymentsGVR, labels: applicationSelector, keep: keepAs[platformstate.Deployment]},
	{name: "deployments-argocd", gvr: deploymentsGVR, namespace: argocdNamespace, keep: keepAs[platformstate.Deployment]},
	{name: "deployments-kube-system", gvr: deploymentsGVR, namespace: kubeSystemNamespace, keep: keepAs[platformstate.Deployment]},
	{name: "pods", gvr: podsGVR, labels: applicationSelector, keep: keepAs[platformstate.Pod]},
	{name: "pods-argocd", gvr: podsGVR, namespace: argocdNamespace, keep: keepAs[platformstate.Pod]},
	{name: "pods-kube-system", gvr: podsGVR, namespace: kubeSystemNamespace, keep: keepAs[platformstate.Pod]},
	{name: "jobs", gvr: jobsGVR, labels: applicationSelector, keep: keepAs[platformstate.Job]},
	{name: "cronjobs", gvr: cronJobsGVR, labels: applicationSelector, keep: keepAs[platformstate.CronJob]},
	{name: "clusters", gvr: clustersGVR, labels: applicationSelector, keep: keepAs[platformstate.PostgresCluster]},
	{name: "certificates", gvr: certificatesGVR, keep: keepAs[platformstate.Certificate]},
	{name: "ingresses", gvr: ingressesGVR, labels: applicationSelector, keep: keepAs[platformstate.Ingress]},
	{name: "events-argocd", gvr: eventsGVR, namespace: argocdNamespace, keep: keepEvent},
	{name: "events-warnings", gvr: eventsGVR, fields: "type=Warning,metadata.namespace!=" + argocdNamespace, keep: keepEvent},
	{name: "nodes", gvr: nodesGVR, keep: keepAs[platformstate.Node]},
}

// kept is what an informer's cache holds for one object: the name and
// namespace the cache keys it by, and platformstate's cut-down struct.
type kept struct {
	metav1.ObjectMeta
	Object any
}

// transform is a source's TransformFunc. It is idempotent, as client-go
// asks: an object already kept is returned as it is. An object that does
// not decode is logged and kept empty, so one odd object cannot stop an
// informer.
func transform(src source, log *slog.Logger) cache.TransformFunc {
	return func(obj any) (any, error) {
		switch o := obj.(type) {
		case *unstructured.Unstructured:
			k := &kept{ObjectMeta: metav1.ObjectMeta{Name: o.GetName(), Namespace: o.GetNamespace(), UID: o.GetUID(), ResourceVersion: o.GetResourceVersion()}}
			v, err := src.keep(o)
			if err != nil {
				log.Warn("an object Argus cannot read is left out", "source", src.name, "namespace", o.GetNamespace(), "name", o.GetName(), "error", err.Error())
				return k, nil
			}
			k.Object = v
			return k, nil
		case cache.DeletedFinalStateUnknown:
			if u, ok := o.Obj.(*unstructured.Unstructured); ok {
				inner, err := transform(src, log)(u)
				return cache.DeletedFinalStateUnknown{Key: o.Key, Obj: inner}, err
			}
		}
		return obj, nil
	}
}

// keepAs decodes an object into platformstate's struct T, keeping only
// the annotations platformstate reads.
func keepAs[T any](u *unstructured.Unstructured) (any, error) {
	var out T
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &out); err != nil {
		return nil, err
	}
	pruneAnnotations(&out)
	return out, nil
}

func keepArgoCD(u *unstructured.Unstructured) (any, error) {
	return keepAs[platformstate.ArgoCDApplication](u)
}

// coreEvent is a core v1 Event as the API serves it: the same object as
// the events.k8s.io/v1 Event the Deploy gate creates, with the older
// field names.
type coreEvent struct {
	Metadata       platformstate.ObjectMeta `json:"metadata"`
	Reason         string                   `json:"reason"`
	Message        string                   `json:"message"`
	Type           string                   `json:"type"`
	EventTime      *time.Time               `json:"eventTime"`
	LastTimestamp  *time.Time               `json:"lastTimestamp"`
	InvolvedObject struct {
		Kind      string `json:"kind"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"involvedObject"`
}

// keepEvent keeps a core v1 Event as platformstate's events.k8s.io/v1
// Event: message is note, involvedObject is regarding, and lastTimestamp
// is deprecatedLastTimestamp.
func keepEvent(u *unstructured.Unstructured) (any, error) {
	var e coreEvent
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &e); err != nil {
		return nil, err
	}
	out := platformstate.Event{
		Metadata: e.Metadata, Reason: e.Reason, Note: e.Message, Type: e.Type,
		EventTime: e.EventTime, DeprecatedLastTimestamp: e.LastTimestamp,
	}
	out.Regarding.Kind, out.Regarding.Namespace, out.Regarding.Name = e.InvolvedObject.Kind, e.InvolvedObject.Namespace, e.InvolvedObject.Name
	pruneAnnotations(&out)
	return out, nil
}

// pruneAnnotations drops every annotation platformstate does not read:
// ArgoCD's tracking id, Traefik's middlewares, and iidp's own. Others,
// such as kubectl's last-applied-configuration, can be as large as the
// object.
func pruneAnnotations(obj any) {
	var m *platformstate.ObjectMeta
	switch o := obj.(type) {
	case *platformstate.ArgoCDApplication:
		m = &o.Metadata
	case *platformstate.Deployment:
		m = &o.Metadata
	case *platformstate.Pod:
		m = &o.Metadata
	case *platformstate.Job:
		m = &o.Metadata
	case *platformstate.CronJob:
		m = &o.Metadata
	case *platformstate.PostgresCluster:
		m = &o.Metadata
	case *platformstate.Certificate:
		m = &o.Metadata
	case *platformstate.Ingress:
		m = &o.Metadata
	case *platformstate.Event:
		m = &o.Metadata
	case *platformstate.Node:
		m = &o.Metadata
	default:
		panic(fmt.Sprintf("pruneAnnotations: %T is not one of platformstate's objects", obj))
	}
	var keep map[string]string
	for k, v := range m.Annotations {
		if strings.HasPrefix(k, "iidp.itema.no/") || k == argus.TrackingAnnotation || k == platformstate.MiddlewaresAnnotation {
			if keep == nil {
				keep = map[string]string{}
			}
			keep[k] = v
		}
	}
	m.Annotations = keep
}

// startInformers starts one informer per source, each putting what it
// keeps into store, until ctx ends. It returns them, to wait for their
// first lists.
func startInformers(ctx context.Context, client dynamic.Interface, store *argus.Store, log *slog.Logger) []cache.SharedIndexInformer {
	var informers []cache.SharedIndexInformer
	for _, src := range sources {
		inf := newInformer(client, src)
		if err := inf.SetTransform(transform(src, log)); err != nil {
			panic(err) // only before Run
		}
		_ = inf.SetWatchErrorHandlerWithContext(func(_ context.Context, _ *cache.Reflector, err error) {
			log.Warn("a watch failed; client-go retries it", "source", src.name, "error", err.Error())
		})
		put := func(obj any) {
			if k, ok := obj.(*kept); ok && k.Object != nil {
				store.Put(src.name, k.Namespace+"/"+k.Name, k.Object)
			}
		}
		_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    put,
			UpdateFunc: func(_, obj any) { put(obj) },
			DeleteFunc: func(obj any) {
				if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					obj = d.Obj
				}
				if k, ok := obj.(*kept); ok {
					store.Delete(src.name, k.Namespace+"/"+k.Name)
				}
			},
		})
		go inf.RunWithContext(ctx)
		informers = append(informers, inf)
	}
	return informers
}

// newInformer is src's informer over the dynamic client. It is what
// dynamicinformer.NewFilteredDynamicInformer builds, written out: that
// package imports k8s.io/client-go/informers for one interface, and with
// it every typed informer, lister and clientset, which would triple the
// binary for code Argus never runs.
func newInformer(client dynamic.Interface, src source) cache.SharedIndexInformer {
	resource := client.Resource(src.gvr).Namespace(src.namespace)
	narrow := func(o metav1.ListOptions) metav1.ListOptions {
		o.LabelSelector, o.FieldSelector = src.labels, src.fields
		return o
	}
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			return resource.List(ctx, narrow(o))
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			return resource.Watch(ctx, narrow(o))
		},
	}
	return cache.NewSharedIndexInformerWithOptions(
		cache.ToListWatcherWithWatchListSemantics(lw, client),
		&unstructured.Unstructured{},
		cache.SharedIndexInformerOptions{ObjectDescription: src.name},
	)
}

// waitForSync waits until every informer has listed once, or until
// timeout: one that cannot list (a CRD not installed yet, a permission
// missing) must not keep the rest from being shown. It logs those that
// have not.
func waitForSync(ctx context.Context, informers []cache.SharedIndexInformer, timeout time.Duration, log *slog.Logger) {
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for i, inf := range informers {
		if !cache.WaitForCacheSync(wait.Done(), inf.HasSynced) {
			log.Warn("an informer has not listed yet; Argus shows the rest", "source", sources[i].name)
		}
	}
}
