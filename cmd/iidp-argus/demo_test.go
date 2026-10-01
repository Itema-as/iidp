package main

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/argus"
	"github.com/Itema-as/iidp/internal/platformstate"
)

// TestDemo serves Argus's page from the real store and server, fed with a
// synthetic Platform instead of informers, and changes that Platform on a
// script, so that the page can be looked at in a browser without a
// cluster. It runs only when asked:
//
//	IIDP_ARGUS_DEMO=127.0.0.1:8090 go test ./cmd/iidp-argus -run TestDemo -timeout 0
//
//	IIDP_ARGUS_DEMO_SCENARIO  tour (the default): every state, and a
//	                          script of Deploys, a Promote, a refusal, a
//	                          supersession, arrivals, departures and a
//	                          lost cluster, round and round;
//	                          still: every state, and nothing moving;
//	                          thirty: 15 Applications with 30
//	                          Environments and a Deploy every few
//	                          seconds, for measuring frame rates
//	IIDP_ARGUS_DEMO_WEB       serve the web directory from this path
//	                          instead of the embedded one, to edit and
//	                          reload
//
// Changes can also be made one at a time, whatever the scenario, with
// GETs (env is an Environment's namespace, such as hello-prod):
//
//	/demo/deploy?env=&tag=[&promote=1][&migrate=1][&fail=1]  a Deploy through its hops
//	/demo/refuse?env=                a refused Deploy
//	/demo/supersede?env=             two Deploys, the first superseded
//	/demo/degrade?env=               crash-looping, and back
//	/demo/arrive?app=                a new Application, then its first Deploy
//	/demo/remove?env=                an Environment leaving
//	/demo/burst                      three places at once
//	/demo/cluster                    the API server stops answering, and back
//	/demo/expire                     every other request answers as oauth2-proxy
//	                                 does once the Itema login session has
//	                                 expired, a 302 to the sign-in page, and back
func TestDemo(t *testing.T) {
	addr := os.Getenv("IIDP_ARGUS_DEMO")
	if addr == "" {
		t.Skip("set IIDP_ARGUS_DEMO to a listen address to serve the demo")
	}
	var files fs.FS
	if dir := os.Getenv("IIDP_ARGUS_DEMO_WEB"); dir != "" {
		files = os.DirFS(dir)
	} else {
		sub, err := fs.Sub(web, "web")
		if err != nil {
			t.Fatal(err)
		}
		files = sub
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := argus.NewStore(time.Now, log)
	store.SetPlatform(argus.NewPlatform("https://argocd.demo.invalid", "https://grafana.demo.invalid",
		"https://github.com/Itema-as/iidp-platform.git", "https://github.com/Itema-as/iidp.git", "v0.0.0"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go store.Run(ctx, time.Second)

	d := &demo{t: t, store: store, envs: map[string]*demoEnv{}}
	d.platform()
	scenario := os.Getenv("IIDP_ARGUS_DEMO_SCENARIO")
	switch scenario {
	case "", "tour", "still":
		d.everyState()
	case "thirty":
		d.thirty()
	default:
		t.Fatalf("no scenario %q", scenario)
	}
	store.Seed()

	// While the session is "expired", every request but /demo/'s gets
	// oauth2-proxy's redirect, and the streams open when it expired end,
	// so that their browsers reconnect into the redirect.
	var mu sync.Mutex
	expired := false
	open := map[*http.Request]context.CancelFunc{}
	handler := (&argus.Server{Store: store, Web: files}).Handler()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /demo/expire", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		expired = !expired
		if expired {
			for _, cancel := range open {
				cancel()
			}
		}
		fmt.Fprintf(w, "expired: %v\n", expired)
	})
	d.controls(ctx, mux)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if expired {
			mu.Unlock()
			http.Redirect(w, r, "https://login.demo.invalid/authorize?state="+r.URL.Path, http.StatusFound)
			return
		}
		rctx, cancel := context.WithCancel(r.Context())
		open[r] = cancel
		mu.Unlock()
		defer func() {
			mu.Lock()
			delete(open, r)
			mu.Unlock()
			cancel()
		}()
		handler.ServeHTTP(w, r.WithContext(rctx))
	}))
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		server.Close()
	}()
	t.Logf("Argus demo (%s) at http://%s", orDefault(scenario, "tour"), addr)
	switch scenario {
	case "", "tour":
		go d.tour(ctx)
	case "thirty":
		go d.busy(ctx)
	}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

type object = map[string]any

// demo is the synthetic Platform: the objects the informers would hold,
// kept as the API server writes them and put into the store decoded, as
// the transforms would.
type demo struct {
	t     *testing.T
	store *argus.Store
	mu    sync.Mutex
	envs  map[string]*demoEnv
	seq   int
}

// demoEnv is one Environment's objects.
type demoEnv struct {
	app, env, ns string
	argo         object
	tag          string
	history      []any
	pods         []string
	postgres     bool
	previewTag   string
	crashing     bool
}

func (d *demo) commit() string {
	d.seq++
	return fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprint("commit", d.seq))))
}

func ago(dur time.Duration) string { return time.Now().Add(-dur).UTC().Format(time.RFC3339) }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// put decodes each object by its kind and puts it in the store.
func (d *demo) put(objects ...object) {
	for _, o := range objects {
		m := o["metadata"].(object)
		ns, _ := m["namespace"].(string)
		key := ns + "/" + m["name"].(string)
		data, err := json.Marshal(o)
		if err != nil {
			d.t.Fatal(err)
		}
		into := func(v any) any {
			if err := json.Unmarshal(data, v); err != nil {
				d.t.Fatalf("decoding %s: %v", data, err)
			}
			return v
		}
		switch o["kind"] {
		case "Application":
			d.store.Put("applications", key, *into(&platformstate.ArgoCDApplication{}).(*platformstate.ArgoCDApplication))
		case "Deployment":
			d.store.Put("deployments", key, *into(&platformstate.Deployment{}).(*platformstate.Deployment))
		case "Pod":
			d.store.Put("pods", key, *into(&platformstate.Pod{}).(*platformstate.Pod))
		case "Job":
			d.store.Put("jobs", key, *into(&platformstate.Job{}).(*platformstate.Job))
		case "CronJob":
			d.store.Put("cronjobs", key, *into(&platformstate.CronJob{}).(*platformstate.CronJob))
		case "Cluster":
			d.store.Put("clusters", key, *into(&platformstate.PostgresCluster{}).(*platformstate.PostgresCluster))
		case "Certificate":
			d.store.Put("certificates", key, *into(&platformstate.Certificate{}).(*platformstate.Certificate))
		case "Ingress":
			d.store.Put("ingresses", key, *into(&platformstate.Ingress{}).(*platformstate.Ingress))
		case "Event":
			d.store.Put("events", key, *into(&platformstate.Event{}).(*platformstate.Event))
		case "Node":
			d.store.Put("nodes", key, *into(&platformstate.Node{}).(*platformstate.Node))
		default:
			d.t.Fatalf("no informer for %v", o["kind"])
		}
	}
}

// componentApp is a Platform component's ArgoCD Application.
func componentApp(name string, images ...string) object {
	return object{
		"kind": "Application",
		"metadata": object{"name": name, "namespace": "argocd", "creationTimestamp": ago(30 * 24 * time.Hour),
			"annotations": object{"argocd.argoproj.io/tracking-id": "platform-components:argoproj.io/Application:argocd/" + name}},
		"spec":   object{"destination": object{"namespace": name}},
		"status": object{"sync": object{"status": "Synced"}, "health": object{"status": "Healthy"}, "summary": object{"images": images}},
	}
}

func workloadDeployment(ns, name, image string, labels, annotations object) object {
	return object{
		"kind": "Deployment",
		"metadata": object{"name": name, "namespace": ns, "generation": 1, "creationTimestamp": ago(24 * time.Hour),
			"labels": labels, "annotations": annotations},
		"spec": object{"replicas": 1, "selector": object{"matchLabels": object{"app.kubernetes.io/instance": name}},
			"template": object{"spec": object{"containers": []any{object{"name": name, "image": image}}}}},
		"status": object{"observedGeneration": 1, "replicas": 1, "updatedReplicas": 1, "readyReplicas": 1, "availableReplicas": 1,
			"conditions": []any{object{"type": "Available", "status": "True", "lastTransitionTime": ago(time.Hour)},
				object{"type": "Progressing", "status": "True", "reason": "NewReplicaSetAvailable", "lastTransitionTime": ago(time.Hour)}}},
	}
}

func workloadPod(ns, name, deployment, image string, ready bool, waiting string) object {
	status := "False"
	if ready {
		status = "True"
	}
	state := object{"running": object{"startedAt": ago(time.Hour)}}
	if waiting != "" {
		state = object{"waiting": object{"reason": waiting}}
	}
	return object{
		"kind": "Pod",
		"metadata": object{"name": name, "namespace": ns, "creationTimestamp": ago(time.Hour),
			"labels": object{"app.kubernetes.io/instance": deployment}},
		"spec": object{"containers": []any{object{"name": deployment, "image": image}}},
		"status": object{"phase": "Running",
			"conditions":        []any{object{"type": "PodScheduled", "status": "True"}, object{"type": "Ready", "status": status, "lastTransitionTime": ago(time.Hour)}},
			"containerStatuses": []any{object{"name": deployment, "image": image, "ready": ready, "restartCount": 0, "state": state}}},
	}
}

// platform is the Platform's components: ArgoCD's Applications for the
// bootstrap's, their workloads in argocd, and Traefik and k3s.
func (d *demo) platform() {
	d.put(
		componentApp("argocd", "quay.io/argoproj/argocd:v3.1.8", "redis:7.4.2-alpine"),
		componentApp("deploy-gate", "ghcr.io/itema-as/iidp-deploy-gate:0.9.0"),
		componentApp("argus", "ghcr.io/itema-as/iidp-argus:0.9.0"),
		componentApp("cert-manager", "quay.io/jetstack/cert-manager-controller:v1.18.2", "quay.io/jetstack/cert-manager-webhook:v1.18.2"),
		componentApp("cloudnative-pg", "ghcr.io/cloudnative-pg/cloudnative-pg:1.27.0"),
		componentApp("cnpg-barman-cloud", "ghcr.io/cloudnative-pg/plugin-barman-cloud:v0.6.0"),
		componentApp("external-dns", "registry.k8s.io/external-dns/external-dns:v0.18.0"),
		componentApp("guardrails"),
		componentApp("monitoring", "grafana/alloy:v1.10.0", "ghcr.io/jimmidyson/configmap-reload:v0.15.0"),
		componentApp("oauth2-proxy", "quay.io/oauth2-proxy/oauth2-proxy:v7.12.0"),
		componentApp("platform-tls"),
		workloadDeployment("argocd", "argocd-server", "quay.io/argoproj/argocd:v3.1.8", nil,
			object{"argocd.argoproj.io/tracking-id": "argocd:apps/Deployment:argocd/argocd-server"}),
		workloadPod("argocd", "argocd-server-0", "argocd-server", "quay.io/argoproj/argocd:v3.1.8", true, ""),
		workloadDeployment("argocd", "iidp-deploy-gate", "ghcr.io/itema-as/iidp-deploy-gate:0.9.0", nil,
			object{"argocd.argoproj.io/tracking-id": "deploy-gate:apps/Deployment:argocd/iidp-deploy-gate"}),
		workloadPod("argocd", "iidp-deploy-gate-0", "iidp-deploy-gate", "ghcr.io/itema-as/iidp-deploy-gate:0.9.0", true, ""),
		workloadDeployment("kube-system", "traefik", "rancher/mirrored-library-traefik:3.3.6", nil, nil),
		workloadPod("kube-system", "traefik-0", "traefik", "rancher/mirrored-library-traefik:3.3.6", true, ""),
		workloadDeployment("kube-system", "coredns", "rancher/mirrored-coredns-coredns:1.12.1", nil, nil),
		workloadPod("kube-system", "coredns-0", "coredns", "rancher/mirrored-coredns-coredns:1.12.1", true, ""),
		object{"kind": "Node", "metadata": object{"name": "iidp", "creationTimestamp": ago(60 * 24 * time.Hour)},
			"status": object{"conditions": []any{object{"type": "Ready", "status": "True", "lastTransitionTime": ago(24 * time.Hour)}},
				"nodeInfo": object{"kubeletVersion": "v1.36.4+k3s1"}}},
	)
}

// envOpts are an Environment's Capabilities and state.
type envOpts struct {
	tag        string // empty: no image yet
	created    time.Duration
	postgres   bool
	login      bool
	domain     string
	task       string
	previewTag string
}

// environment puts app env on the Platform, serving opts.tag.
func (d *demo) environment(app, env string, o envOpts) *demoEnv {
	ns := app + "-" + env
	if o.created == 0 {
		o.created = 30 * 24 * time.Hour
	}
	c := d.commit()
	e := &demoEnv{app: app, env: env, ns: ns, tag: o.tag, postgres: o.postgres, previewTag: o.previewTag}
	e.history = []any{object{"id": 1, "revisions": []any{"0.5.0", c}, "deployedAt": ago(26 * time.Hour)}}
	host := app + ".app.itma.no"
	if env != "prod" {
		host = app + "-" + env + ".app.itma.no"
	}
	urls := []any{"https://" + host}
	if o.domain != "" {
		urls = append(urls, "https://"+o.domain)
	}
	spec := object{"destination": object{"namespace": ns}}
	if o.previewTag != "" {
		spec["sources"] = []any{object{"helm": object{"valuesObject": object{"image": object{"tag": o.previewTag}}}}}
	}
	e.argo = object{
		"kind": "Application",
		"metadata": object{"name": ns, "namespace": "argocd", "creationTimestamp": ago(o.created),
			"labels": object{"iidp.itema.no/application": app, "iidp.itema.no/environment": env}},
		"spec": spec,
		"status": object{
			"sync": object{"status": "Synced", "revisions": []any{"0.5.0", c}}, "health": object{"status": "Healthy"},
			"reconciledAt": ago(time.Minute),
			"operationState": object{"phase": "Succeeded", "startedAt": ago(26 * time.Hour), "finishedAt": ago(26 * time.Hour),
				"operation": object{"sync": object{"revisions": []any{"0.5.0", c}}}, "syncResult": object{"revisions": []any{"0.5.0", c}}},
			"history": e.history,
			"summary": object{"externalURLs": urls},
		},
	}
	d.envs[ns] = e
	d.put(e.argo)
	labels := object{"iidp.itema.no/application": app, "iidp.itema.no/environment": env}
	if o.tag != "" {
		d.put(workloadDeployment(ns, app, "ghcr.io/itema-as/"+app+":"+o.tag, labels, nil))
		d.pod(e, "a", o.tag, true, "")
	}
	if o.postgres {
		d.put(object{"kind": "Cluster", "metadata": object{"name": app + "-db", "namespace": ns, "creationTimestamp": ago(o.created), "labels": labels},
			"status": object{"phase": "Cluster in healthy state", "conditions": []any{
				object{"type": "Ready", "status": "True", "lastTransitionTime": ago(time.Hour)},
				object{"type": "LastBackupSucceeded", "status": "True"}}}})
	}
	if o.login || o.domain != "" {
		annotations := object{}
		if o.login {
			annotations["traefik.ingress.kubernetes.io/router.middlewares"] = "oauth2-proxy-itema-login-auth@kubernetescrd"
		}
		d.put(object{"kind": "Ingress", "metadata": object{"name": app, "namespace": ns, "labels": labels, "annotations": annotations}})
	}
	if o.domain != "" {
		d.put(object{"kind": "Certificate", "metadata": object{"name": o.domain, "namespace": ns, "creationTimestamp": ago(o.created)},
			"spec": object{"dnsNames": []any{o.domain}},
			"status": object{"notAfter": time.Now().Add(60 * 24 * time.Hour).UTC().Format(time.RFC3339),
				"conditions": []any{object{"type": "Ready", "status": "True", "lastTransitionTime": ago(time.Hour)}}}})
	}
	if o.task != "" {
		taskLabels := object{"iidp.itema.no/application": app, "iidp.itema.no/environment": env,
			"app.kubernetes.io/component": "scheduled-task", "iidp.itema.no/task": o.task}
		d.put(object{"kind": "CronJob", "metadata": object{"name": app + "-" + o.task, "namespace": ns, "labels": taskLabels},
			"spec": object{"schedule": "0 3 * * *"}, "status": object{"lastScheduleTime": ago(7 * time.Hour)}},
			object{"kind": "Job", "metadata": object{"name": app + "-" + o.task + "-1", "namespace": ns, "labels": taskLabels, "creationTimestamp": ago(7 * time.Hour)},
				"status": object{"startTime": ago(7 * time.Hour), "completionTime": ago(7*time.Hour - time.Minute),
					"conditions": []any{object{"type": "Complete", "status": "True"}}}})
	}
	return e
}

// crash makes e's pods crash-loop, which is Degraded at once, or serve
// again.
func (d *demo) crash(e *demoEnv, on bool) {
	waiting := ""
	if on {
		waiting = "CrashLoopBackOff"
	}
	for _, name := range e.pods {
		d.put(workloadPod(e.ns, name, e.app, "ghcr.io/itema-as/"+e.app+":"+e.tag, !on, waiting))
	}
}

// pod puts a pod of e running tag.
func (d *demo) pod(e *demoEnv, suffix, tag string, ready bool, waiting string) {
	name := e.app + "-" + suffix
	d.put(workloadPod(e.ns, name, e.app, "ghcr.io/itema-as/"+e.app+":"+tag, ready, waiting))
	for _, p := range e.pods {
		if p == name {
			return
		}
	}
	e.pods = append(e.pods, name)
}

func (d *demo) status(e *demoEnv) object { return e.argo["status"].(object) }

// everyState is a Platform showing every state.
func (d *demo) everyState() {
	d.environment("shop", "prod", envOpts{tag: "2.4.0", postgres: true, login: true})
	d.environment("shop", "staging", envOpts{tag: "2.5.0", postgres: true, login: true})
	d.environment("shop", "pr-42", envOpts{tag: "pr-42-9f1c2d3", login: true, previewTag: "pr-42-9f1c2d3"})
	d.environment("iprofil", "prod", envOpts{tag: "1.8.2", postgres: true, domain: "iprofil.no"})
	d.environment("iprofil", "staging", envOpts{tag: "1.9.0", postgres: true})
	d.environment("hello", "prod", envOpts{tag: "1.0.0"})
	d.environment("wiki", "prod", envOpts{created: 2 * time.Hour})

	api := d.environment("api", "prod", envOpts{tag: "3.1.0", postgres: true})
	d.pod(api, "a", "3.1.0", false, "CrashLoopBackOff")
	d.put(object{"kind": "Cluster", "metadata": object{"name": "api-db", "namespace": "api-prod", "creationTimestamp": ago(time.Hour),
		"labels": object{"iidp.itema.no/application": "api", "iidp.itema.no/environment": "prod"}},
		"status": object{"phase": "Cluster in healthy state", "conditions": []any{
			object{"type": "Ready", "status": "True", "lastTransitionTime": ago(time.Hour)},
			object{"type": "LastBackupSucceeded", "status": "False", "reason": "LastBackupFailed", "message": "barman-cloud-backup exited with status 2"}}}})
	d.environment("api", "staging", envOpts{tag: "3.2.0", postgres: true})

	d.environment("timesheet", "prod", envOpts{tag: "5.0.1", postgres: true, task: "nightly-export"})
	ts := d.environment("timesheet", "staging", envOpts{tag: "5.0.1", postgres: true})
	d.stuckDeploy(ts, "5.1.0")

	booking := d.environment("booking", "prod", envOpts{tag: "0.9.3", login: true, domain: "booking.itema.no"})
	unknown := workloadPod(booking.ns, "booking-a", "booking", "ghcr.io/itema-as/booking:0.9.3", false, "")
	unknown["status"].(object)["phase"] = "Unknown"
	d.put(unknown)
	staging := d.environment("booking", "staging", envOpts{tag: "0.9.4", login: true})
	d.updating(staging)
}

// stuckDeploy is a Deploy of tag to e whose migration failed.
func (d *demo) stuckDeploy(e *demoEnv, tag string) {
	c := d.commit()
	d.gateEvent(e, "DeployAccepted", "deploy", tag, c, "")
	st := d.status(e)
	st["sync"] = object{"status": "OutOfSync", "revisions": []any{"0.5.0", c}}
	st["operationState"] = object{"phase": "Failed", "message": "one or more synchronization tasks completed unsuccessfully", "startedAt": now(), "finishedAt": now(),
		"operation": object{"sync": object{"revisions": []any{"0.5.0", c}}}}
	d.put(e.argo, d.migration(e, tag, "Failed"))
}

// updating is e's desired state changing without a new image: a secret
// set, being synced.
func (d *demo) updating(e *demoEnv) {
	c := d.commit()
	st := d.status(e)
	st["sync"] = object{"status": "OutOfSync", "revisions": []any{"0.5.0", c}}
	st["operationState"] = object{"phase": "Running", "startedAt": now(), "operation": object{"sync": object{"revisions": []any{"0.5.0", c}}}}
	d.put(e.argo)
}

func (d *demo) settle(e *demoEnv) {
	c := d.commit()
	st := d.status(e)
	e.history = append(e.history, object{"id": len(e.history) + 1, "revisions": []any{"0.5.0", c}, "deployedAt": now()})
	st["history"] = e.history
	st["sync"] = object{"status": "Synced", "revisions": []any{"0.5.0", c}}
	st["operationState"] = object{"phase": "Succeeded", "startedAt": now(), "finishedAt": now(),
		"operation": object{"sync": object{"revisions": []any{"0.5.0", c}}}, "syncResult": object{"revisions": []any{"0.5.0", c}}}
	d.put(e.argo)
}

func (d *demo) migration(e *demoEnv, tag, phase string) object {
	job := object{"kind": "Job",
		"metadata": object{"name": e.app + "-migrate-" + strings.ReplaceAll(tag, ".", "-"), "namespace": e.ns, "creationTimestamp": now(),
			"labels": object{"iidp.itema.no/application": e.app, "iidp.itema.no/environment": e.env, "app.kubernetes.io/component": "migration"}},
		"spec":   object{"template": object{"spec": object{"containers": []any{object{"name": "migrate", "image": "ghcr.io/itema-as/" + e.app + ":" + tag}}}}},
		"status": object{"startTime": now()},
	}
	switch phase {
	case "Failed":
		job["status"].(object)["conditions"] = []any{object{"type": "Failed", "status": "True"}}
	case "Complete":
		job["status"].(object)["completionTime"] = now()
		job["status"].(object)["conditions"] = []any{object{"type": "Complete", "status": "True"}}
	}
	return job
}

func (d *demo) gateEvent(e *demoEnv, reason, kind, tag, commit, note string) {
	d.seq++
	annotations := object{"iidp.itema.no/application": e.app, "iidp.itema.no/environment": e.env, "iidp.itema.no/tag": tag, "iidp.itema.no/kind": kind}
	typ := "Normal"
	if commit != "" {
		annotations["iidp.itema.no/commit"] = commit
	}
	if reason == "DeployRefused" {
		typ = "Warning"
	}
	if note == "" {
		note = fmt.Sprintf("Deploy %s %s %s accepted", e.app, e.env, tag)
	}
	d.put(object{"kind": "Event",
		"metadata": object{"name": fmt.Sprintf("%s.%d", e.ns, d.seq), "namespace": "argocd", "creationTimestamp": now(), "annotations": annotations},
		"reason":   reason, "note": note, "type": typ, "eventTime": time.Now().UTC().Format("2006-01-02T15:04:05.000000Z07:00"),
		"regarding": object{"kind": "Application", "namespace": "argocd", "name": e.ns}})
}

// deploy runs a Deploy (or a Promote) of tag to e through its five hops,
// a few seconds each; with fail, its migration fails and it stays stuck
// at Applying.
func (d *demo) deploy(ctx context.Context, e *demoEnv, tag string, promote, migrate, fail bool) {
	step := func(dur time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(dur):
			return true
		}
	}
	kind := "deploy"
	if promote {
		kind = "promote"
	}
	d.mu.Lock()
	c := d.commit()
	d.gateEvent(e, "DeployAccepted", kind, tag, c, fmt.Sprintf("%s %s %s %s accepted", strings.ToUpper(kind[:1])+kind[1:], e.app, e.env, tag))
	d.mu.Unlock()
	if !step(3 * time.Second) {
		return
	}
	d.mu.Lock()
	st := d.status(e)
	st["reconciledAt"] = now()
	st["sync"] = object{"status": "OutOfSync", "revisions": []any{"0.5.0", c}}
	d.put(e.argo)
	d.mu.Unlock()
	if !step(3 * time.Second) {
		return
	}
	d.mu.Lock()
	st["operationState"] = object{"phase": "Running", "startedAt": now(), "operation": object{"sync": object{"revisions": []any{"0.5.0", c}}}}
	d.put(e.argo)
	if migrate {
		d.put(d.migration(e, tag, "Running"))
	}
	d.mu.Unlock()
	if !step(4 * time.Second) {
		return
	}
	d.mu.Lock()
	if fail {
		st["operationState"] = object{"phase": "Failed", "message": "one or more synchronization tasks completed unsuccessfully", "startedAt": now(), "finishedAt": now(),
			"operation": object{"sync": object{"revisions": []any{"0.5.0", c}}}}
		d.put(e.argo, d.migration(e, tag, "Failed"))
		d.mu.Unlock()
		return
	}
	if migrate {
		d.put(d.migration(e, tag, "Complete"))
	}
	e.history = append(e.history, object{"id": len(e.history) + 1, "revisions": []any{"0.5.0", c}, "deployedAt": now()})
	st["history"] = e.history
	st["sync"] = object{"status": "Synced", "revisions": []any{"0.5.0", c}}
	st["operationState"] = object{"phase": "Succeeded", "startedAt": now(), "finishedAt": now(),
		"operation": object{"sync": object{"revisions": []any{"0.5.0", c}}}, "syncResult": object{"revisions": []any{"0.5.0", c}}}
	d.put(e.argo)
	labels := object{"iidp.itema.no/application": e.app, "iidp.itema.no/environment": e.env}
	rolling := workloadDeployment(e.ns, e.app, "ghcr.io/itema-as/"+e.app+":"+tag, labels, nil)
	rolling["metadata"].(object)["generation"] = 2
	rolling["status"] = object{"observedGeneration": 2, "replicas": 2, "updatedReplicas": 1, "readyReplicas": 1, "availableReplicas": 1,
		"conditions": []any{object{"type": "Progressing", "status": "True", "reason": "ReplicaSetUpdated", "lastTransitionTime": now()}}}
	d.put(rolling)
	d.pod(e, "n"+fmt.Sprint(d.seq), tag, false, "")
	d.mu.Unlock()
	if !step(3 * time.Second) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range e.pods {
		d.store.Delete("pods", e.ns+"/"+p)
	}
	e.pods = nil
	e.tag = tag
	d.put(workloadDeployment(e.ns, e.app, "ghcr.io/itema-as/"+e.app+":"+tag, labels, nil))
	d.pod(e, "s"+fmt.Sprint(d.seq), tag, true, "")
}

// remove takes e off the Platform: Leaving, with the final backup when it
// has a database, then gone.
func (d *demo) remove(ctx context.Context, e *demoEnv) {
	d.mu.Lock()
	e.argo["metadata"].(object)["deletionTimestamp"] = now()
	d.put(e.argo)
	if e.postgres {
		d.put(object{"kind": "Job", "metadata": object{"name": e.app + "-final-backup", "namespace": e.ns, "creationTimestamp": now(),
			"labels": object{"iidp.itema.no/application": e.app, "iidp.itema.no/environment": e.env, "app.kubernetes.io/component": "final-backup"}},
			"status": object{"startTime": now()}})
	}
	d.mu.Unlock()
	select {
	case <-ctx.Done():
		return
	case <-time.After(7 * time.Second):
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.store.Delete("applications", "argocd/"+e.ns)
	delete(d.envs, e.ns)
}

// tour changes the Platform on a loop, one thing every few seconds, so
// that every animation and every camera rule shows.
func (d *demo) tour(ctx context.Context) {
	wait := func(dur time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(dur):
			return true
		}
	}
	env := func(ns string) *demoEnv {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.envs[ns]
	}
	minor := 1
	for round := 1; ctx.Err() == nil; round++ {
		if !wait(6 * time.Second) {
			return
		}
		// A Deploy through its five hops.
		minor++
		d.deploy(ctx, env("hello-prod"), fmt.Sprintf("1.%d.0", minor), false, true, false)
		if !wait(8 * time.Second) {
			return
		}
		// A refused Deploy.
		d.mu.Lock()
		d.gateEvent(d.envs["shop-prod"], "DeployRefused", "deploy", "9.9.9", "",
			"refused: the image ghcr.io/itema-as/shop:9.9.9 does not exist in the registry")
		d.mu.Unlock()
		if !wait(8 * time.Second) {
			return
		}
		// A new Application arrives, and gets its first image.
		name := fmt.Sprintf("ledger%d", round)
		d.mu.Lock()
		arriving := d.environment(name, "prod", envOpts{created: time.Second, postgres: true})
		d.mu.Unlock()
		if !wait(6 * time.Second) {
			return
		}
		d.deploy(ctx, arriving, "0.1.0", false, true, false)
		if !wait(8 * time.Second) {
			return
		}
		// A Promote: staging's version moves on to prod.
		d.mu.Lock()
		stagingTag := d.envs["shop-staging"].tag
		d.mu.Unlock()
		d.deploy(ctx, env("shop-prod"), stagingTag, true, true, false)
		if !wait(6 * time.Second) {
			return
		}
		// Two Deploys in quick succession: the first is superseded.
		d.mu.Lock()
		first := d.commit()
		d.gateEvent(d.envs["shop-staging"], "DeployAccepted", "deploy", fmt.Sprintf("2.6.%d", round), first, "")
		d.mu.Unlock()
		if !wait(2 * time.Second) {
			return
		}
		d.deploy(ctx, env("shop-staging"), fmt.Sprintf("2.6.%d-1", round), false, false, false)
		if !wait(8 * time.Second) {
			return
		}
		// A Deploy gets stuck on its migration.
		d.deploy(ctx, env("iprofil-staging"), fmt.Sprintf("1.9.%d", round), false, true, true)
		if !wait(10 * time.Second) {
			return
		}
		// Several things at once: a wide shot.
		d.mu.Lock()
		d.crash(d.envs["hello-prod"], true)
		d.updating(d.envs["iprofil-prod"])
		c := d.componentUpdating("cert-manager")
		d.mu.Unlock()
		go d.deploy(ctx, env("api-staging"), fmt.Sprintf("3.2.%d", round), false, false, false)
		if !wait(14 * time.Second) {
			return
		}
		d.mu.Lock()
		d.crash(d.envs["hello-prod"], false)
		d.settle(d.envs["iprofil-prod"])
		d.put(c)
		d.mu.Unlock()
		if !wait(8 * time.Second) {
			return
		}
		// The new Application leaves again, with its final backup.
		d.remove(ctx, arriving)
		if !wait(6 * time.Second) {
			return
		}
		// A Platform component degrades and recovers.
		d.mu.Lock()
		d.put(workloadPod("kube-system", "traefik-0", "traefik", "rancher/mirrored-library-traefik:3.3.6", false, "CrashLoopBackOff"))
		d.mu.Unlock()
		if !wait(12 * time.Second) {
			return
		}
		d.mu.Lock()
		d.put(workloadPod("kube-system", "traefik-0", "traefik", "rancher/mirrored-library-traefik:3.3.6", true, ""))
		d.mu.Unlock()
		if round%3 == 0 {
			d.store.SetReachable(false)
			if !wait(45 * time.Second) {
				return
			}
			d.store.SetReachable(true)
		}
	}
}

// componentUpdating makes a component Updating, and returns it settled.
func (d *demo) componentUpdating(name string) object {
	c := componentApp(name, "quay.io/jetstack/cert-manager-controller:v1.18.3")
	st := c["status"].(object)
	st["sync"] = object{"status": "OutOfSync"}
	st["operationState"] = object{"phase": "Running", "startedAt": now(), "operation": object{"sync": object{"revision": "v1.18.3"}}}
	d.put(c)
	return componentApp(name, "quay.io/jetstack/cert-manager-controller:v1.18.3")
}

var thirtyNames = []string{"shop", "iprofil", "hello", "api", "wiki", "timesheet", "booking", "ledger", "kantine", "roomsy", "fakturo", "radar", "intranet", "onboard", "kudos"}

// thirty is 15 Applications, each with prod and staging.
func (d *demo) thirty() {
	for i, name := range thirtyNames {
		d.environment(name, "prod", envOpts{tag: "1.0.0", postgres: i%2 == 0, login: i%3 == 0, domain: map[bool]string{true: name + ".no"}[i%5 == 0], task: map[bool]string{true: "report"}[i%4 == 0]})
		d.environment(name, "staging", envOpts{tag: "1.1.0", postgres: i%2 == 0, login: i%3 == 0})
	}
}

// busy deploys somewhere every few seconds, and now and then degrades
// something, so that the thirty keep moving.
func (d *demo) busy(ctx context.Context) {
	for n := 2; ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(4 * time.Second):
		}
		d.mu.Lock()
		name := thirtyNames[rand.IntN(len(thirtyNames))]
		e := d.envs[name+"-staging"]
		d.mu.Unlock()
		go d.deploy(ctx, e, fmt.Sprintf("1.%d.0", n), false, n%2 == 0, n%11 == 0)
	}
}

// controls are the /demo/ endpoints that make one change each.
func (d *demo) controls(ctx context.Context, mux *http.ServeMux) {
	env := func(w http.ResponseWriter, r *http.Request) *demoEnv {
		d.mu.Lock()
		defer d.mu.Unlock()
		e := d.envs[r.URL.Query().Get("env")]
		if e == nil {
			http.Error(w, "no such Environment", http.StatusNotFound)
		}
		return e
	}
	flag := func(r *http.Request, name string) bool { return r.URL.Query().Get(name) == "1" }
	mux.HandleFunc("GET /demo/deploy", func(w http.ResponseWriter, r *http.Request) {
		if e := env(w, r); e != nil {
			tag := orDefault(r.URL.Query().Get("tag"), fmt.Sprintf("9.%d.0", time.Now().Unix()%1000))
			go d.deploy(ctx, e, tag, flag(r, "promote"), flag(r, "migrate"), flag(r, "fail"))
			fmt.Fprintln(w, "deploying", tag)
		}
	})
	mux.HandleFunc("GET /demo/refuse", func(w http.ResponseWriter, r *http.Request) {
		if e := env(w, r); e != nil {
			d.mu.Lock()
			d.gateEvent(e, "DeployRefused", "deploy", "9.9.9", "", "refused: the image ghcr.io/itema-as/"+e.app+":9.9.9 does not exist in the registry")
			d.mu.Unlock()
		}
	})
	mux.HandleFunc("GET /demo/supersede", func(w http.ResponseWriter, r *http.Request) {
		if e := env(w, r); e != nil {
			d.mu.Lock()
			d.gateEvent(e, "DeployAccepted", "deploy", "8.0.0", d.commit(), "")
			d.mu.Unlock()
			go func() {
				time.Sleep(2 * time.Second)
				d.deploy(ctx, e, "8.0.1", false, false, false)
			}()
		}
	})
	mux.HandleFunc("GET /demo/degrade", func(w http.ResponseWriter, r *http.Request) {
		if e := env(w, r); e != nil {
			d.mu.Lock()
			defer d.mu.Unlock()
			e.crashing = !e.crashing
			d.crash(e, e.crashing)
			fmt.Fprintln(w, "crash-looping:", e.crashing)
		}
	})
	mux.HandleFunc("GET /demo/arrive", func(w http.ResponseWriter, r *http.Request) {
		name := orDefault(r.URL.Query().Get("app"), "ledger")
		d.mu.Lock()
		e := d.environment(name, "prod", envOpts{created: time.Second, postgres: true})
		d.mu.Unlock()
		go func() {
			time.Sleep(6 * time.Second)
			d.deploy(ctx, e, "0.1.0", false, true, false)
		}()
	})
	mux.HandleFunc("GET /demo/remove", func(w http.ResponseWriter, r *http.Request) {
		if e := env(w, r); e != nil {
			go d.remove(ctx, e)
		}
	})
	mux.HandleFunc("GET /demo/burst", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, ns := range []string{"hello-prod", "iprofil-prod", "shop-staging"} {
			if e := d.envs[ns]; e != nil {
				d.updating(e)
			}
		}
		if e := d.envs["api-staging"]; e != nil {
			e.crashing = true
			d.crash(e, true)
		}
	})
	reachable := true
	mux.HandleFunc("GET /demo/cluster", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		reachable = !reachable
		d.store.SetReachable(reachable)
		fmt.Fprintln(w, "reachable:", reachable)
	})
}
