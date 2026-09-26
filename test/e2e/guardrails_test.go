//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// validationFailure is the audit annotation a ValidatingAdmissionPolicy
// binding with the Audit action puts on the request's audit event
// (kubernetes.io/docs/reference/labels-annotations-taints/audit-annotations).
const validationFailure = "validation.policy.admission.k8s.io/validation_failure"

// fixtureNamespaces are the Application namespaces the fixture Platform
// repository's Environments are installed into.
var fixtureNamespaces = []string{"shop-prod", "shop-staging", "brochure-prod", "later-prod"}

// testGuardrails proves #90 on the cluster:
//   - every fixture Environment's namespace carries the labels the CLI
//     writes through managedNamespaceMetadata: the Application namespace
//     label the guardrails' bindings select on, and Pod Security's levels;
//   - nothing the fixture Applications did during the whole test (the
//     Deployments, Services and Ingresses, CloudNativePG's database Pods and
//     Jobs with the Barman Cloud sidecar, the migration Jobs, the Scheduled
//     task's Jobs, shop-staging's final Backup hook, brochure's first
//     deploy) failed a guardrail: no
//     audit event in a fixture namespace carries a validation failure;
//   - a NodePort Service applied by hand in a fixture namespace is warned
//     about and audited, and still created: the bindings Warn and Audit,
//     they do not Deny yet;
//   - Pod Security's baseline is enforced: a privileged Pod is refused.
func testGuardrails(ctx context.Context, t *testing.T, cluster *Cluster) {
	t.Helper()

	for _, ns := range fixtureNamespaces {
		out, err := cluster.Kubectl(ctx, "get", "namespace", ns, "-o", "jsonpath={.metadata.labels}")
		if err != nil {
			// shop-staging was deleted by testDeleteEnvironment, and ArgoCD
			// does not delete the namespace it created.
			t.Fatalf("read namespace %s: %v\n%s", ns, err, out)
		}
		var labels map[string]string
		if err := json.Unmarshal([]byte(out), &labels); err != nil {
			t.Fatalf("namespace %s labels %q: %v", ns, out, err)
		}
		application, environment, _ := strings.Cut(ns, "-")
		for key, want := range map[string]string{
			"iidp.itema.no/application":          application,
			"iidp.itema.no/environment":          environment,
			"pod-security.kubernetes.io/enforce": "baseline",
			"pod-security.kubernetes.io/warn":    "restricted",
			"pod-security.kubernetes.io/audit":   "restricted",
		} {
			if labels[key] != want {
				t.Errorf("namespace %s label %s = %q, want %q", ns, key, labels[key], want)
			}
		}
	}

	events, err := cluster.AuditLog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inFixture := map[string]bool{}
	for _, ns := range fixtureNamespaces {
		inFixture[ns] = true
	}
	seen := 0
	for _, e := range events {
		if !inFixture[e.ObjectRef.Namespace] {
			continue
		}
		seen++
		if failure, ok := e.Annotations[validationFailure]; ok {
			t.Errorf("%s %s/%s in %s failed a guardrail: %s", e.Verb, e.ObjectRef.Resource, e.ObjectRef.Name, e.ObjectRef.Namespace, failure)
		}
	}
	// The audit policy logs writes to Pods, Services, Ingresses and the
	// workloads; the fixture namespaces saw dozens of them. None would mean
	// the log is not being written, and the check above proved nothing.
	if seen < 10 {
		t.Fatalf("the audit log has %d events in the fixture namespaces, want the fixture Applications' writes", seen)
	}
	t.Logf("%d audited writes in the fixture namespaces, none failing a guardrail", seen)

	// A deliberate violation: warned, audited, not denied.
	const name = "guardrails-e2e-nodeport"
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: shop-prod
spec:
  type: NodePort
  selector:
    app.kubernetes.io/name: nothing
  ports:
    - port: 80
`, name)
	out, err := cluster.ApplyOutput(ctx, manifest)
	if err != nil {
		t.Fatalf("a NodePort Service was refused; the guardrails should only warn and audit for now: %v\n%s", err, out)
	}
	defer func() {
		if out, err := cluster.Kubectl(context.WithoutCancel(ctx), "-n", "shop-prod", "delete", "service", name, "--ignore-not-found"); err != nil {
			t.Logf("delete service %s: %v\n%s", name, err, out)
		}
	}()
	if !strings.Contains(out, "Warning:") || !strings.Contains(out, "iidp-service-types") {
		t.Errorf("applying a NodePort Service printed no iidp-service-types warning:\n%s", out)
	}
	if exists, err := cluster.ResourceExists(ctx, "service", "shop-prod", name); err != nil || !exists {
		t.Errorf("the NodePort Service does not exist after apply (err %v)", err)
	}
	err = pollUntil(ctx, 30*time.Second, 2*time.Second, func() (bool, error) {
		events, err := cluster.AuditLog(ctx)
		if err != nil {
			return false, err
		}
		for _, e := range events {
			if e.ObjectRef.Resource != "services" || e.ObjectRef.Namespace != "shop-prod" || e.ObjectRef.Name != name {
				continue
			}
			failure := e.Annotations[validationFailure]
			if strings.Contains(failure, "iidp-service-types") && strings.Contains(failure, `"Warn"`) && strings.Contains(failure, `"Audit"`) && e.ResponseStatus.Code < 300 {
				t.Logf("audited: %s %s/%s -> %d, %s", e.Verb, e.ObjectRef.Namespace, e.ObjectRef.Name, e.ResponseStatus.Code, failure)
				return true, nil
			}
		}
		return false, nil
	}, func() error {
		return fmt.Errorf("no audit event for Service shop-prod/%s carries an iidp-service-types validation failure with the Warn and Audit actions and a successful response", name)
	})
	if err != nil {
		t.Error(err)
	}

	// Pod Security enforces baseline: a privileged Pod never gets in. A
	// server-side dry run goes through admission and creates nothing.
	privileged := `apiVersion: v1
kind: Pod
metadata:
  name: guardrails-e2e-privileged
  namespace: shop-prod
spec:
  containers:
    - name: app
      image: docker.io/library/nginx:1.30-alpine
      resources:
        limits: {cpu: 10m, memory: 16Mi}
      securityContext:
        privileged: true
`
	out, err = cluster.ApplyOutput(ctx, privileged, "--dry-run=server")
	if err == nil || !strings.Contains(out, `violates PodSecurity "baseline`) {
		t.Errorf("a privileged Pod in shop-prod was not refused by Pod Security baseline: err %v\n%s", err, out)
	}
}
