package platform_test

// Tests for #90: cloud-init turns on the API server's audit log, where the
// guardrails' admission policies and Pod Security record what they
// report (their Audit action), before k3s first starts. The two renders in
// registries_test.go call assertAuditLog.

import (
	"encoding/base64"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	auditPolicyFile   = "cloud-init/audit-policy.yaml"
	auditPolicyPath   = "/etc/rancher/k3s/audit-policy.yaml"
	auditDropInPath   = "/etc/rancher/k3s/config.yaml.d/50-iidp-audit.yaml"
	auditLogPath      = "/var/lib/rancher/k3s/server/logs/audit.log"
	auditPolicyAPIVer = "audit.k8s.io/v1"
)

func readAuditPolicy(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(auditPolicyFile)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The audit policy logs, at Metadata level (so the admission annotations
// and never an object's body), the writes to every kind the guardrails
// and Pod Security check, and nothing else.
func TestAuditPolicyLogsWhatTheGuardrailsCheck(t *testing.T) {
	var policy struct {
		APIVersion string   `yaml:"apiVersion"`
		Kind       string   `yaml:"kind"`
		OmitStages []string `yaml:"omitStages"`
		Rules      []struct {
			Level     string   `yaml:"level"`
			Verbs     []string `yaml:"verbs"`
			Resources []struct {
				Group     string   `yaml:"group"`
				Resources []string `yaml:"resources"`
			} `yaml:"resources"`
		} `yaml:"rules"`
	}
	if err := yaml.Unmarshal(readAuditPolicy(t), &policy); err != nil {
		t.Fatalf("%s is not valid YAML: %v", auditPolicyFile, err)
	}
	if policy.APIVersion != auditPolicyAPIVer || policy.Kind != "Policy" {
		t.Fatalf("%s is %s %s, want %s Policy", auditPolicyFile, policy.APIVersion, policy.Kind, auditPolicyAPIVer)
	}
	if len(policy.Rules) != 2 {
		t.Fatalf("%s has %d rules, want the Metadata rule and a final None", auditPolicyFile, len(policy.Rules))
	}
	logged, last := policy.Rules[0], policy.Rules[1]
	if logged.Level != "Metadata" {
		t.Errorf("first rule level = %q, want Metadata: the annotations without the bodies", logged.Level)
	}
	if got := strings.Join(logged.Verbs, ","); got != "create,update,patch" {
		t.Errorf("first rule verbs = %s, want create,update,patch", got)
	}
	got := map[string]bool{}
	for _, r := range logged.Resources {
		for _, name := range r.Resources {
			got[r.Group+"/"+name] = true
		}
	}
	for _, want := range []string{
		"/pods", "/services", "networking.k8s.io/ingresses",
		"apps/deployments", "apps/replicasets", "apps/statefulsets", "apps/daemonsets",
		"batch/jobs", "batch/cronjobs",
	} {
		if !got[want] {
			t.Errorf("the audit policy does not log writes to %s", want)
		}
	}
	if got["/secrets"] || got["/configmaps"] {
		t.Errorf("the audit policy logs Secrets or ConfigMaps; it only needs what the guardrails check")
	}
	if last.Level != "None" || len(last.Resources) != 0 || len(last.Verbs) != 0 {
		t.Errorf("the last rule is %+v, want a catch-all level None", last)
	}
	if !slices.Contains(policy.OmitStages, "RequestReceived") {
		t.Errorf("omitStages = %v, want RequestReceived omitted (one event per request, not two)", policy.OmitStages)
	}
}

// assertAuditLog checks a rendered cloud-config: the audit policy, byte
// for byte the one the kind harness uses too, and the k3s config drop-in
// that points kube-apiserver at it, both written by write_files (before
// runcmd starts k3s) and root-only.
func assertAuditLog(t *testing.T, rendered string) {
	t.Helper()
	var cfg cloudConfig
	if err := yaml.Unmarshal([]byte(rendered), &cfg); err != nil {
		t.Fatalf("the rendered cloud-config is not valid YAML: %v", err)
	}
	files := map[string][]byte{}
	for _, f := range cfg.WriteFiles {
		if f.Path != auditPolicyPath && f.Path != auditDropInPath {
			continue
		}
		if _, dup := files[f.Path]; dup {
			t.Fatalf("write_files writes %s twice", f.Path)
		}
		if f.Permissions != "0600" || f.Owner != "root:root" || f.Defer {
			t.Errorf("%s is %s %s (defer %t), want 0600 root:root written before k3s starts", f.Path, f.Permissions, f.Owner, f.Defer)
		}
		content := []byte(f.Content)
		if f.Encoding == "b64" {
			var err error
			if content, err = base64.StdEncoding.DecodeString(f.Content); err != nil {
				t.Fatalf("%s content is not valid base64: %v", f.Path, err)
			}
		}
		files[f.Path] = content
	}
	if got, want := string(files[auditPolicyPath]), string(readAuditPolicy(t)); got != want {
		t.Errorf("%s = %q, want %s byte for byte", auditPolicyPath, got, auditPolicyFile)
	}

	var dropIn map[string][]string
	if err := yaml.Unmarshal(files[auditDropInPath], &dropIn); err != nil {
		t.Fatalf("%s is not valid YAML: %v\n%s", auditDropInPath, err, files[auditDropInPath])
	}
	// "+" appends to kube-apiserver-arg from any other config file rather
	// than replacing it (k3s's configuration file docs, "Value Merge
	// Behavior").
	args, ok := dropIn["kube-apiserver-arg+"]
	if !ok || len(dropIn) != 1 {
		t.Fatalf("%s = %v, want only kube-apiserver-arg+", auditDropInPath, dropIn)
	}
	for _, want := range []string{
		"audit-policy-file=" + auditPolicyPath,
		"audit-log-path=" + auditLogPath,
		"audit-log-maxsize=50",
		"audit-log-maxbackup=4",
		"audit-log-maxage=30",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("%s lacks %q: %v", auditDropInPath, want, args)
		}
	}
}
