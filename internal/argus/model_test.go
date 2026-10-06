package argus

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/platformstate"
)

// An Environment's objects are what its namespace holds, whatever their
// labels: certificates included, which cert-manager makes from the
// chart's Ingress. The Deploy gate's Events about it are its Deploys.
func TestAnEnvironmentHasItsNamespacesObjects(t *testing.T) {
	s, _ := newStore(t)
	cert := obj{"kind": "Certificate", "metadata": obj{"name": "shop-www", "namespace": "shop-prod", "creationTimestamp": at(-time.Hour)},
		"spec":   obj{"dnsNames": []any{"www.shop.example"}},
		"status": obj{"notAfter": at(60 * 24 * time.Hour), "conditions": []any{obj{"type": "Ready", "status": "True"}}}}
	login := obj{"kind": "Ingress", "metadata": obj{"name": "shop", "namespace": "shop-prod",
		"annotations": obj{platformstate.MiddlewaresAnnotation: "oauth2-proxy-itema-login-auth@kubernetescrd"}}}
	put(t, s, servingShop()...)
	put(t, s, cert, login,
		gateEvent("shop", "prod", platformstate.ReasonDeployAccepted, "deploy", "1.0.1", "2222222222222222222222222222222222222222", "", -time.Minute),
		// notes's objects stay notes's.
		argoApp("notes", "prod"), envDeployment("notes", "prod", "3.0.0"), pod("notes-prod", "notes-a", "notes", "3.0.0", true, -time.Hour),
		gateEvent("notes", "prod", platformstate.ReasonDeployAccepted, "deploy", "3.0.1", "3333333333333333333333333333333333333333", "", -time.Minute),
	)
	s.Seed()
	snap := s.Snapshot()
	if len(snap.Applications) != 2 {
		t.Fatalf("Applications = %+v", snap.Applications)
	}
	shop := snap.Applications[1].Environments[0]
	var caps []string
	for _, c := range shop.Capabilities {
		caps = append(caps, c.Type+":"+c.Name)
	}
	if strings.Join(caps, " ") != "itema-login:itema-login custom-domain:www.shop.example" {
		t.Errorf("shop prod Capabilities = %v", caps)
	}
	if shop.Image == nil || shop.Image.Tag != "1.0.0" || shop.Pods.Ready != 1 {
		t.Errorf("shop prod = image %+v, pods %+v", shop.Image, shop.Pods)
	}
	if len(shop.Deploys) != 1 || shop.Deploys[0].Tag != "1.0.1" || shop.Deploys[0].Hop != platformstate.HopAccepted ||
		shop.Activity == nil || shop.Activity.State != platformstate.Deploying {
		t.Errorf("shop prod Deploys = %+v, Activity %+v; want 1.0.1 accepted, Deploying", shop.Deploys, shop.Activity)
	}
	notes := snap.Applications[0].Environments[0]
	if notes.Image == nil || notes.Image.Tag != "3.0.0" || len(notes.Capabilities) != 0 || len(notes.Deploys) != 1 || notes.Deploys[0].Tag != "3.0.1" {
		t.Errorf("notes prod = %+v", notes)
	}
}

// ArgoCD's own OutOfSync time is not recorded; the store records when it
// saw an Environment turn OutOfSync, and platformstate counts the 5
// minutes from then.
func TestOutOfSyncIsTimedFromWhenArgusSawIt(t *testing.T) {
	s, clock := newStore(t)
	put(t, s, servingShop()...)
	s.Seed()
	outOfSync := argoApp("shop", "prod")
	outOfSync["status"].(obj)["sync"] = obj{"status": "OutOfSync", "revisions": []any{"0.4.0", commit1}}
	clock.add(time.Hour)
	put(t, s, outOfSync)
	s.Recompute()
	activity := func() *platformstate.Activity { return s.Snapshot().Applications[0].Environments[0].Activity }
	if a := activity(); a == nil || a.State != platformstate.Updating || a.Stuck {
		t.Fatalf("Activity = %+v just after turning OutOfSync, want Updating", a)
	}
	clock.add(5*time.Minute + time.Second)
	s.Recompute()
	if a := activity(); a == nil || !a.Stuck {
		t.Errorf("Activity = %+v 5 minutes on, want stuck", a)
	}
}

// The Platform components: the ArgoCD Applications platform-components
// manages, judged by their Deployments and pods in argocd and
// kube-system when Argus sees them and by ArgoCD's health when it does
// not; Traefik; and k3s, the node and the rest of kube-system.
func TestPlatformComponents(t *testing.T) {
	s, _ := newStore(t)
	tracked := func(app, kind, ns, name string) obj {
		return obj{"argocd.argoproj.io/tracking-id": app + ":apps/" + kind + ":" + ns + "/" + name}
	}
	degraded := component("cert-manager")
	degraded["status"].(obj)["health"] = obj{"status": "Degraded"}
	root := component("platform-components")
	root["metadata"].(obj)["annotations"] = obj{"argocd.argoproj.io/tracking-id": "platform:argoproj.io/Application:argocd/platform-components"}
	controller := pod("argocd", "argocd-application-controller-0", "argocd-application-controller", "v3.5.3", false, -2*time.Minute)
	controller["metadata"].(obj)["labels"] = obj{"app.kubernetes.io/part-of": "argocd"}
	crashing := pod(kubeSystem, "coredns-1", "coredns", "1.12", false, -10*time.Second)
	crashing["status"].(obj)["containerStatuses"] = []any{obj{"name": "coredns", "ready": false, "state": obj{"waiting": obj{"reason": "CrashLoopBackOff"}}}}
	svclb := pod(kubeSystem, "svclb-traefik-1", "svclb", "v0.13", true, -time.Hour)
	svclb["metadata"].(obj)["labels"] = obj{"svccontroller.k3s.cattle.io/svcname": "traefik"}
	put(t, s,
		component("argocd"), component("deploy-gate"), degraded, component("oauth2-proxy"), root,
		deployment("argocd", "argocd-server", "v3.5.3", nil, tracked("argocd", "Deployment", "argocd", "argocd-server")),
		pod("argocd", "argocd-server-1", "argocd-server", "v3.5.3", true, -time.Hour),
		controller,
		deployment("argocd", "iidp-deploy-gate", "1.2.3", nil, tracked("deploy-gate", "Deployment", "argocd", "iidp-deploy-gate")),
		pod("argocd", "iidp-deploy-gate-1", "iidp-deploy-gate", "1.2.3", true, -time.Hour),
		deployment(kubeSystem, "traefik", "3.7.8", nil, nil),
		pod(kubeSystem, "traefik-1", "traefik", "3.7.8", true, -time.Hour),
		svclb,
		deployment(kubeSystem, "coredns", "1.12", nil, nil),
		crashing,
		obj{"kind": "Node", "metadata": obj{"name": "iidp", "namespace": "", "creationTimestamp": at(-30 * 24 * time.Hour)},
			"status": obj{"conditions": []any{obj{"type": "Ready", "status": "True", "lastTransitionTime": at(-24 * time.Hour)}}}},
	)
	s.Seed()
	var got []string
	for _, c := range s.Snapshot().Components {
		got = append(got, fmt.Sprintf("%s:%s:%s", c.Name, c.Condition.State, c.Condition.Reason))
	}
	want := []string{
		"argocd:Degraded:0 of 1 pods ready for over a minute",
		"cert-manager:Degraded:ArgoCD reports its health as Degraded",
		"deploy-gate:Healthy:",
		"k3s:Degraded:coredns-1 is in CrashLoopBackOff",
		"oauth2-proxy:Healthy:",
		"traefik:Healthy:",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("components:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Only cmd/iidp-argus links client-go, and only its dynamic client: the CLI
// and the Deploy gate keep their standard-library Kubernetes client.
func TestOnlyArgusLinksClientGo(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}
	out, err := exec.Command(gobin, "list", "-deps", "github.com/Itema-as/iidp/cmd/iidp", "github.com/Itema-as/iidp/cmd/iidp-deploy-gate", "github.com/Itema-as/iidp/internal/argus").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	var k8s []string
	for _, pkg := range strings.Fields(string(out)) {
		if strings.HasPrefix(pkg, "k8s.io/") || strings.HasPrefix(pkg, "sigs.k8s.io/") {
			k8s = append(k8s, pkg)
		}
	}
	sort.Strings(k8s)
	if len(k8s) > 0 {
		t.Errorf("the CLI, the Deploy gate or internal/argus link %v; only cmd/iidp-argus may", k8s)
	}

	// And Argus itself none of the typed clientsets or API types.
	out, err = exec.Command(gobin, "list", "-deps", "github.com/Itema-as/iidp/cmd/iidp-argus").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		for _, banned := range []string{"k8s.io/client-go/kubernetes", "k8s.io/client-go/informers", "k8s.io/api/", "github.com/argoproj/", "github.com/cloudnative-pg/"} {
			if strings.HasPrefix(pkg, banned) {
				t.Errorf("cmd/iidp-argus links %s: only the dynamic client is used", pkg)
			}
		}
	}
}

// Argus shows each Environment's database access as its Cluster's
// annotations have it, and a Preview Environment's the same way; it reads
// nothing more than the Clusters it already watches.
func TestAnEnvironmentsDatabaseAccessComesFromItsCluster(t *testing.T) {
	s, _ := newStore(t)
	cluster := func(ns, name, readWrite, readOnly string, open ...string) obj {
		var roles []any
		for _, role := range open {
			roles = append(roles, obj{"name": role, "ensure": "present"})
		}
		return obj{"kind": "Cluster", "metadata": obj{"name": name, "namespace": ns, "creationTimestamp": at(-time.Hour),
			"labels": obj{platformstate.ApplicationLabel: "shop"},
			"annotations": obj{
				platformstate.DatabaseAccessReadWriteAnnotation: readWrite,
				platformstate.DatabaseAccessReadOnlyAnnotation:  readOnly,
			}},
			"spec": obj{"managed": obj{"roles": roles}}}
	}
	put(t, s, servingShop()...)
	put(t, s, argoApp("shop", "pr-4"), envDeployment("shop", "pr-4", "abc"),
		cluster("shop-prod", "shop-db", "none", "maintain", "shop_read"), cluster("shop-pr-4", "shop-pr-4-db", "push", "none"))
	s.Seed()
	envs := map[string]*platformstate.DatabaseAccess{}
	for _, env := range s.Snapshot().Applications[0].Environments {
		envs[env.Name] = env.DatabaseAccess
	}
	for name, want := range map[string]platformstate.DatabaseAccess{
		"prod": {ReadWrite: "none", ReadOnly: "maintain", ReadWriteSetUp: true, ReadOnlySetUp: true},
		// staging's read-write password was never written.
		"pr-4": {ReadWrite: "push", ReadOnly: "none", ReadOnlySetUp: true},
	} {
		if envs[name] == nil || *envs[name] != want {
			t.Errorf("%s database access = %+v, want %+v", name, envs[name], want)
		}
	}
}
