// Package e2e stands up a kind cluster the way the Platform node is
// bootstrapped (Traefik as k3s ships it, ArgoCD from the pinned chart, the
// age key Secret and the root Application), with the repositories served by
// an in-cluster git server. No cloud account is involved.
package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// GitServerNamespace holds the in-cluster git server.
	GitServerNamespace = "iidp-e2e"
	// GitBaseURL is where the served repositories are reachable from inside
	// the cluster: <GitBaseURL>/<name>.git. The fixture Platform repository
	// hardcodes it.
	GitBaseURL = "http://git-server." + GitServerNamespace + ".svc.cluster.local/git"

	gitServerImage = "iidp-e2e.local/git-server:dev"

	// objectStorageImage is the harness-only S3 stand-in, so the final
	// Backup PreDelete hook can really complete in kind. versitygw rather
	// than MinIO because MinIO's images no longer allow anonymous pulls.
	objectStorageImage = "ghcr.io/versity/versitygw:v1.8.0@sha256:30292fc2eeacc67a36993b01f7a7a5e3361a19cced0e80c1d71cfa2a4b0a2499"
	// ObjectStorageAccessKey and ObjectStorageSecretKey are the stand-in's
	// root credentials (not secret: the cluster never leaves the machine).
	// They must match what the fixture's backups-credentials.enc.yaml files
	// decrypt to.
	ObjectStorageAccessKey = "iidpe2e"
	ObjectStorageSecretKey = "iidpe2epassword"
	// ObjectStorageBucket is the bucket InstallObjectStorage creates. It and
	// the stand-in's address,
	// http://object-storage.<GitServerNamespace>.svc.cluster.local:7070, are
	// hardcoded in the fixture Platform repository.
	ObjectStorageBucket = "iidp-backups"

	// kind has no ServiceLB, so Traefik's entrypoints are NodePorts that the
	// kind config maps to host ports.
	traefikWebNodePort       = 30080
	traefikWebsecureNodePort = 30443

	auditPolicyPath = "/etc/kubernetes/iidp-audit-policy.yaml"
	// AuditLogPath is where the API server writes the audit log, on the
	// kind node.
	AuditLogPath = "/var/log/kubernetes/audit/audit.log"
)

// Versions is the part of bootstrap/versions.yaml the harness needs.
type Versions struct {
	ArgoCD struct {
		Chart      string `yaml:"chart"`
		Repository string `yaml:"repository"`
	} `yaml:"argocd"`
	KSOPS struct {
		Image string `yaml:"image"`
	} `yaml:"ksops"`
	K8sMonitoring struct {
		Chart      string `yaml:"chart"`
		Repository string `yaml:"repository"`
	} `yaml:"k8sMonitoring"`
	K3s struct {
		Version string `yaml:"version"`
		Traefik struct {
			Chart       string `yaml:"chart"`
			CRDChartURL string `yaml:"crdChartURL"`
			ChartURL    string `yaml:"chartURL"`
			Image       string `yaml:"image"`
		} `yaml:"traefik"`
		HelmController string `yaml:"helmController"`
	} `yaml:"k3s"`
	Kind struct {
		NodeImage string `yaml:"nodeImage"`
	} `yaml:"kind"`
}

// LoadVersions reads bootstrap/versions.yaml below the repository root.
func LoadVersions(repoRoot string) (Versions, error) {
	var v Versions
	data, err := os.ReadFile(filepath.Join(repoRoot, "bootstrap", "versions.yaml"))
	if err != nil {
		return v, err
	}
	if err := yaml.Unmarshal(data, &v); err != nil {
		return v, fmt.Errorf("parse bootstrap/versions.yaml: %w", err)
	}
	for name, value := range map[string]string{
		"argocd.chart":             v.ArgoCD.Chart,
		"argocd.repository":        v.ArgoCD.Repository,
		"ksops.image":              v.KSOPS.Image,
		"k8sMonitoring.chart":      v.K8sMonitoring.Chart,
		"k8sMonitoring.repository": v.K8sMonitoring.Repository,
		"k3s.traefik.chart":        v.K3s.Traefik.Chart,
		"k3s.traefik.chartURL":     v.K3s.Traefik.ChartURL,
		"k3s.traefik.crdChartURL":  v.K3s.Traefik.CRDChartURL,
		"k3s.traefik.image":        v.K3s.Traefik.Image,
		"k3s.helmController":       v.K3s.HelmController,
		"kind.nodeImage":           v.Kind.NodeImage,
	} {
		if value == "" {
			return v, fmt.Errorf("bootstrap/versions.yaml: %s is empty", name)
		}
	}
	return v, nil
}

// RepoRoot finds the directory holding go.mod above the working directory.
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above the working directory")
		}
		dir = parent
	}
}

// Cluster is one kind cluster prepared like the Platform node.
type Cluster struct {
	Name       string
	Kubeconfig string
	// HTTPPort and HTTPSPort are the host ports mapped to Traefik's web and
	// websecure entrypoints.
	HTTPPort  int
	HTTPSPort int
	// Provider is the container engine kind runs on: podman or docker.
	Provider string
	RepoRoot string
	Versions Versions
	Log      func(format string, args ...any)

	images *imageCache
}

// NewCluster describes a cluster without creating it.
func NewCluster(name string, logf func(format string, args ...any)) (*Cluster, error) {
	root, err := RepoRoot()
	if err != nil {
		return nil, err
	}
	versions, err := LoadVersions(root)
	if err != nil {
		return nil, err
	}
	kubeconfig, err := os.CreateTemp("", "iidp-e2e-kubeconfig-*")
	if err != nil {
		return nil, err
	}
	kubeconfig.Close()
	provider := "docker"
	if os.Getenv("KIND_EXPERIMENTAL_PROVIDER") == "podman" {
		provider = "podman"
	}
	httpPort := 18080
	httpsPort := 18443
	if v := os.Getenv("IIDP_E2E_HTTP_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("IIDP_E2E_HTTP_PORT: %w", err)
		}
		httpPort = p
	}
	if v := os.Getenv("IIDP_E2E_HTTPS_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("IIDP_E2E_HTTPS_PORT: %w", err)
		}
		httpsPort = p
	}
	images, err := openImageCache()
	if err != nil {
		return nil, err
	}
	return &Cluster{
		images:     images,
		Name:       name,
		Kubeconfig: kubeconfig.Name(),
		HTTPPort:   httpPort,
		HTTPSPort:  httpsPort,
		Provider:   provider,
		RepoRoot:   root,
		Versions:   versions,
		Log:        logf,
	}, nil
}

// Create creates the kind cluster with the pinned node image, deleting a
// leftover cluster of the same name first.
func (c *Cluster) Create(ctx context.Context) error {
	out, err := c.run(ctx, "kind", "get", "clusters")
	if err != nil {
		return fmt.Errorf("kind get clusters: %w\n%s", err, out)
	}
	for _, existing := range strings.Fields(out) {
		if existing == c.Name {
			c.Log("deleting leftover kind cluster %s", c.Name)
			if err := c.Delete(ctx); err != nil {
				return err
			}
		}
	}
	// The API server audits with the Platform node's own policy, so the
	// guardrails' Audit action can be observed. kubeadm v1beta4 (Kubernetes
	// 1.31 and later) takes extraArgs as a list of name/value pairs.
	config := fmt.Sprintf(`kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: %d
        hostPort: %d
      - containerPort: %d
        hostPort: %d
    extraMounts:
      - hostPath: %s
        containerPath: %s
        readOnly: true
    kubeadmConfigPatches:
      - |
        kind: ClusterConfiguration
        apiServer:
          extraArgs:
            - name: audit-policy-file
              value: %s
            - name: audit-log-path
              value: %s
          extraVolumes:
            - name: audit-policy
              hostPath: %s
              mountPath: %s
              readOnly: true
              pathType: File
            - name: audit-log
              hostPath: %s
              mountPath: %s
              readOnly: false
              pathType: DirectoryOrCreate
`, traefikWebNodePort, c.HTTPPort, traefikWebsecureNodePort, c.HTTPSPort,
		filepath.Join(c.RepoRoot, "infra", "platform", "cloud-init", "audit-policy.yaml"), auditPolicyPath,
		auditPolicyPath, AuditLogPath,
		auditPolicyPath, auditPolicyPath,
		filepath.Dir(AuditLogPath), filepath.Dir(AuditLogPath))
	configFile, err := writeTemp("iidp-e2e-kind-*.yaml", []byte(config))
	if err != nil {
		return err
	}
	defer os.Remove(configFile)
	nodeImage, err := c.ensureNodeImage(ctx)
	if err != nil {
		return err
	}
	c.Log("creating kind cluster %s from %s (%s)", c.Name, nodeImage, c.Versions.Kind.NodeImage)
	out, err = c.run(ctx, "kind", "create", "cluster",
		"--name", c.Name,
		"--config", configFile,
		"--kubeconfig", c.Kubeconfig,
		"--image", nodeImage,
		"--wait", "120s")
	if err != nil {
		return fmt.Errorf("kind create cluster: %w\n%s", err, out)
	}
	// The whole stack shares a hosted CI runner's 2 vCPUs, so kind's own
	// CoreDNS and local-path provisioner are cut down to what a single-node
	// cluster needs, leaving CPU for the fixture Applications.
	if out, err := c.Kubectl(ctx, "-n", "kube-system", "scale", "deployment/coredns", "--replicas=1"); err != nil {
		return fmt.Errorf("scale coredns: %w\n%s", err, out)
	}
	if out, err := c.Kubectl(ctx, "-n", "kube-system", "set", "resources", "deployment/coredns", "--requests=cpu=10m"); err != nil {
		return fmt.Errorf("trim coredns cpu request: %w\n%s", err, out)
	}
	if out, err := c.Kubectl(ctx, "-n", "local-path-storage", "set", "resources", "deployment/local-path-provisioner", "--requests=cpu=10m"); err != nil {
		return fmt.Errorf("trim local-path-provisioner cpu request: %w\n%s", err, out)
	}
	return nil
}

// Delete removes the kind cluster and the kubeconfig.
func (c *Cluster) Delete(ctx context.Context) error {
	out, err := c.run(ctx, "kind", "delete", "cluster", "--name", c.Name, "--kubeconfig", c.Kubeconfig)
	if err != nil {
		return fmt.Errorf("kind delete cluster: %w\n%s", err, out)
	}
	os.Remove(c.Kubeconfig)
	return nil
}

// Close removes the image cache directory when it is a temporary one. It
// leaves the cluster alone.
func (c *Cluster) Close() {
	c.images.remove()
}

// Kubectl runs kubectl against the cluster and returns its combined output.
func (c *Cluster) Kubectl(ctx context.Context, args ...string) (string, error) {
	return c.run(ctx, "kubectl", args...)
}

// Apply feeds a manifest to kubectl apply.
func (c *Cluster) Apply(ctx context.Context, manifest string, extraArgs ...string) error {
	args := append([]string{"apply", "-f", "-"}, extraArgs...)
	out, err := c.runWithStdin(ctx, strings.NewReader(manifest), "kubectl", args...)
	if err != nil {
		return fmt.Errorf("kubectl apply: %w\n%s", err, out)
	}
	return nil
}

// ApplyOutput feeds a manifest to kubectl apply and returns its combined
// output, admission warnings ("Warning: ...") included.
func (c *Cluster) ApplyOutput(ctx context.Context, manifest string, extraArgs ...string) (string, error) {
	args := append([]string{"apply", "-f", "-"}, extraArgs...)
	return c.runWithStdin(ctx, strings.NewReader(manifest), "kubectl", args...)
}

// AuditEvent is the part of an audit.k8s.io/v1 Event the tests read.
type AuditEvent struct {
	Verb      string `json:"verb"`
	Stage     string `json:"stage"`
	ObjectRef struct {
		Resource  string `json:"resource"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"objectRef"`
	ResponseStatus struct {
		Code int `json:"code"`
	} `json:"responseStatus"`
	Annotations map[string]string `json:"annotations"`
}

// AuditLog reads the API server's audit log off the kind node.
func (c *Cluster) AuditLog(ctx context.Context) ([]AuditEvent, error) {
	cmd := exec.CommandContext(ctx, c.Provider, "exec", c.Name+"-control-plane", "cat", AuditLogPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("read the audit log: %w\n%s", err, stderr.String())
	}
	var events []AuditEvent
	dec := json.NewDecoder(&stdout)
	for dec.More() {
		var e AuditEvent
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("parse the audit log: %w", err)
		}
		events = append(events, e)
	}
	return events, nil
}

// Helm runs helm against the cluster and returns its combined output.
func (c *Cluster) Helm(ctx context.Context, args ...string) (string, error) {
	return c.run(ctx, "helm", args...)
}

// InstallTraefik installs the Traefik chart k3s bundles with the values k3s
// and the bootstrap's HelmChartConfig set, plus NodePorts and the node IP on
// Ingress status in place of the ServiceLB kind lacks. It also installs the
// HelmChartConfig CRD so the bootstrap's HelmChartConfig applies, though
// nothing in kind acts on it.
func (c *Cluster) InstallTraefik(ctx context.Context) error {
	nodeIP, err := c.Kubectl(ctx, "get", "nodes", "-o", `jsonpath={.items[0].status.addresses[?(@.type=="InternalIP")].address}`)
	if err != nil || strings.TrimSpace(nodeIP) == "" {
		return fmt.Errorf("node IP: %v\n%s", err, nodeIP)
	}
	nodeIP = strings.TrimSpace(nodeIP)
	t := c.Versions.K3s.Traefik
	c.Log("installing Traefik chart %s (image %s) as k3s %s does", t.Chart, t.Image, c.Versions.K3s.Version)
	out, err := c.Helm(ctx, "upgrade", "--install", "traefik-crd", t.CRDChartURL,
		"--namespace", "kube-system", "--wait", "--timeout", "5m")
	if err != nil {
		return fmt.Errorf("helm install traefik-crd: %w\n%s", err, out)
	}
	out, err = c.Helm(ctx, "upgrade", "--install", "traefik", t.ChartURL,
		"--namespace", "kube-system",
		"--set-string", "image.tag="+t.Image,
		"--set", "service.type=NodePort",
		"--set", fmt.Sprintf("ports.web.nodePort=%d", traefikWebNodePort),
		"--set", fmt.Sprintf("ports.websecure.nodePort=%d", traefikWebsecureNodePort),
		"--set", "ports.web.http.redirections.entryPoint.to=websecure",
		"--set", "ports.web.http.redirections.entryPoint.scheme=https",
		"--set", "ports.web.http.redirections.entryPoint.permanent=true",
		// k3s publishes the Traefik Service's load balancer address on
		// every Ingress; with a NodePort there is none, so publish the
		// node IP instead.
		"--set", "providers.kubernetesIngress.publishedService.enabled=false",
		"--set", "additionalArguments={--providers.kubernetesingress.ingressendpoint.ip="+nodeIP+"}",
		// Below the chart's 100m default, to fit everything on a 2 vCPU
		// CI runner.
		"--set", "resources.requests.cpu=20m",
		"--set", "resources.requests.memory=64Mi",
		"--wait", "--timeout", "5m")
	if err != nil {
		return fmt.Errorf("helm install traefik: %w\n%s", err, out)
	}
	crd := fmt.Sprintf("https://raw.githubusercontent.com/k3s-io/helm-controller/%s/pkg/crds/yaml/generated/helm.cattle.io_helmchartconfigs.yaml", c.Versions.K3s.HelmController)
	if out, err := c.Kubectl(ctx, "apply", "-f", crd); err != nil {
		return fmt.Errorf("install HelmChartConfig CRD: %w\n%s", err, out)
	}
	return nil
}

// InstallArgoCD installs ArgoCD as cloud-init does: helm template of the
// argo-cd chart, applied server-side, with the values the argocd bootstrap
// Application's takeover relies on (fullnameOverride, crds.install) so its
// first sync only patches what is already running. Then waits for ArgoCD.
func (c *Cluster) InstallArgoCD(ctx context.Context) error {
	a := c.Versions.ArgoCD
	c.Log("installing ArgoCD by rendering the argo-cd chart %s", a.Chart)
	if err := c.Apply(ctx, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: argocd\n"); err != nil {
		return err
	}
	manifest, err := c.argoCDManifest(ctx)
	if err != nil {
		return err
	}
	// Server-side apply is required: the ArgoCD CRDs exceed the annotation
	// size limit of client-side apply.
	if err := c.Apply(ctx, manifest, "-n", "argocd", "--server-side", "--force-conflicts"); err != nil {
		return err
	}
	if out, err := c.Kubectl(ctx, "wait", "--for=condition=established", "crd/applications.argoproj.io", "--timeout=120s"); err != nil {
		return fmt.Errorf("wait for Application CRD: %w\n%s", err, out)
	}
	for _, workload := range []string{"deployment/argocd-repo-server", "deployment/argocd-server", "statefulset/argocd-application-controller"} {
		if out, err := c.Kubectl(ctx, "-n", "argocd", "rollout", "status", workload, "--timeout=5m"); err != nil {
			return fmt.Errorf("wait for %s: %w\n%s", workload, err, out)
		}
	}
	return nil
}

func (c *Cluster) argoCDManifest(ctx context.Context) (string, error) {
	a := c.Versions.ArgoCD
	manifest, err := c.HelmTemplate(ctx, "argocd", "argo-cd",
		"--repo", a.Repository, "--version", a.Chart,
		"--namespace", "argocd", "--include-crds",
		"--set", "fullnameOverride=argocd", "--set", "crds.install=true")
	if err != nil {
		return "", fmt.Errorf("helm template argo-cd: %w", err)
	}
	return manifest, nil
}

// HelmTemplate runs helm template and returns stdout only, so helm's log
// lines on stderr cannot corrupt the YAML.
func (c *Cluster) HelmTemplate(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "helm", append([]string{"template"}, args...)...)
	cmd.Env = c.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("helm template %v: %w\n%s", args, err, stderr.String())
	}
	return stdout.String(), nil
}

// CreateAgeKeySecret creates the Secret argocd/sops-age with keyFile under
// keys.txt, the layout cloud-init produces and KSOPS mounts.
func (c *Cluster) CreateAgeKeySecret(ctx context.Context, keyFile string) error {
	out, err := c.Kubectl(ctx, "-n", "argocd", "create", "secret", "generic", "sops-age", "--from-file=keys.txt="+keyFile)
	if err != nil {
		return fmt.Errorf("create sops-age secret: %w\n%s", err, out)
	}
	return nil
}

// CreateNamespace creates a namespace, doing nothing if it already exists.
func (c *Cluster) CreateNamespace(ctx context.Context, name string) error {
	return c.Apply(ctx, fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", name))
}

// InstallObjectStorage deploys the harness-only S3 stand-in in
// GitServerNamespace with ObjectStorageBucket already in it.
func (c *Cluster) InstallObjectStorage(ctx context.Context) error {
	c.Log("installing the Object Storage stand-in (namespace %s, bucket %s)", GitServerNamespace, ObjectStorageBucket)
	if err := c.CreateNamespace(ctx, GitServerNamespace); err != nil {
		return err
	}
	if err := c.Apply(ctx, objectStorageManifest()); err != nil {
		return err
	}
	if out, err := c.Kubectl(ctx, "-n", GitServerNamespace, "rollout", "status", "deployment/object-storage", "--timeout=2m"); err != nil {
		logs, _ := c.Kubectl(ctx, "-n", GitServerNamespace, "logs", "deployment/object-storage", "--all-containers")
		return fmt.Errorf("wait for object-storage: %w\n%s\n%s", err, out, logs)
	}
	return nil
}

// JobSucceeded reports whether the named Job has succeeded. A missing or
// unfinished Job reports false with no error, so a caller can poll it.
func (c *Cluster) JobSucceeded(ctx context.Context, namespace, name string) (bool, error) {
	out, err := c.Kubectl(ctx, "-n", namespace, "get", "job", name, "-o", "jsonpath={.status.succeeded}")
	if err != nil {
		return false, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return false, nil
	}
	return n >= 1, nil
}

// WaitForJobSucceeded polls JobSucceeded until it reports true or timeout
// elapses.
func (c *Cluster) WaitForJobSucceeded(ctx context.Context, namespace, name string, timeout time.Duration) error {
	return pollUntil(ctx, timeout, 5*time.Second,
		func() (bool, error) { return c.JobSucceeded(ctx, namespace, name) },
		func() error {
			out, _ := c.Kubectl(ctx, "-n", namespace, "get", "job", name, "-o", "yaml")
			return fmt.Errorf("job %s/%s did not succeed within %s:\n%s", namespace, name, timeout, out)
		})
}

// WaitForJobRunning polls until the named Job, running command as its first
// container's sh -c line, has succeeded, then returns its log. Matching on
// command keeps a Sync hook's previous run from being mistaken for the new
// one.
func (c *Cluster) WaitForJobRunning(ctx context.Context, namespace, name, command string, timeout time.Duration) (string, error) {
	var last string
	err := pollUntil(ctx, timeout, 5*time.Second,
		func() (bool, error) {
			out, err := c.Kubectl(ctx, "-n", namespace, "get", "job", name, "-o",
				`jsonpath={.spec.template.spec.containers[0].command[2]}{"\n"}{.status.succeeded}{"\n"}{.status.failed}`)
			if err != nil {
				last = out
				return false, nil
			}
			last = out
			lines := strings.Split(out, "\n")
			if len(lines) < 3 || lines[0] != command {
				return false, nil
			}
			if strings.TrimSpace(lines[2]) != "" {
				logs, _ := c.Kubectl(ctx, "-n", namespace, "logs", "job/"+name)
				return false, fmt.Errorf("job %s/%s running %q failed:\n%s", namespace, name, command, logs)
			}
			return strings.TrimSpace(lines[1]) == "1", nil
		},
		func() error {
			return fmt.Errorf("job %s/%s did not succeed running %q within %s; last seen:\n%s", namespace, name, command, timeout, last)
		})
	if err != nil {
		return "", err
	}
	logs, err := c.Kubectl(ctx, "-n", namespace, "logs", "job/"+name)
	if err != nil {
		return "", fmt.Errorf("logs of job %s/%s: %w\n%s", namespace, name, err, logs)
	}
	return logs, nil
}

// WaitForScheduledTaskRun polls until a Job of the named Scheduled task has
// succeeded and returns its log. A failed run fails the wait at once.
func (c *Cluster) WaitForScheduledTaskRun(ctx context.Context, namespace, task string, timeout time.Duration) (string, error) {
	var last, succeeded string
	err := pollUntil(ctx, timeout, 5*time.Second,
		func() (bool, error) {
			out, err := c.Kubectl(ctx, "-n", namespace, "get", "jobs", "-l", "iidp.itema.no/task="+task, "-o",
				`jsonpath={range .items[*]}{.metadata.name}|{.status.succeeded}|{.status.failed}{"\n"}{end}`)
			last = out
			if err != nil {
				return false, nil
			}
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				fields := strings.Split(line, "|")
				if len(fields) != 3 {
					continue
				}
				name, ok, failed := fields[0], fields[1], fields[2]
				if ok != "" && ok != "0" {
					succeeded = name
					return true, nil
				}
				if failed != "" && failed != "0" {
					logs, _ := c.Kubectl(ctx, "-n", namespace, "logs", "job/"+name)
					return false, fmt.Errorf("scheduled task %s's job %s/%s failed:\n%s", task, namespace, name, logs)
				}
			}
			return false, nil
		},
		func() error {
			cronJob, _ := c.Kubectl(ctx, "-n", namespace, "get", "cronjobs", "-l", "iidp.itema.no/task="+task, "-o", "yaml")
			return fmt.Errorf("no run of scheduled task %s in %s succeeded within %s; jobs:\n%s\ncronjob:\n%s", task, namespace, timeout, last, cronJob)
		})
	if err != nil {
		return "", err
	}
	logs, err := c.Kubectl(ctx, "-n", namespace, "logs", "job/"+succeeded)
	if err != nil {
		return "", fmt.Errorf("logs of job %s/%s: %w\n%s", namespace, succeeded, err, logs)
	}
	return logs, nil
}

// BackupPhases returns every CloudNativePG Backup in namespace with its
// status.phase. A namespace without Backups, or that is gone, yields an
// empty map.
//
// CloudNativePG removes a Backup along with its Cluster, so a final backup
// has to be observed by polling during the deletion, not after it.
func (c *Cluster) BackupPhases(ctx context.Context, namespace string) (map[string]string, error) {
	out, err := c.Kubectl(ctx, "-n", namespace, "get", "backups.postgresql.cnpg.io", "-o",
		`jsonpath={range .items[*]}{.metadata.name}={.status.phase}{"\n"}{end}`)
	if err != nil {
		return map[string]string{}, nil
	}
	phases := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, phase, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || name == "" {
			continue
		}
		phases[name] = phase
	}
	return phases, nil
}

// ResourceExists reports whether the named resource of kind exists in
// namespace.
func (c *Cluster) ResourceExists(ctx context.Context, kind, namespace, name string) (bool, error) {
	out, err := c.Kubectl(ctx, "-n", namespace, "get", kind, name)
	if err == nil {
		return true, nil
	}
	if strings.Contains(out, "NotFound") {
		return false, nil
	}
	// Any other error is inconclusive: report the resource as still there,
	// so a poll-until-gone caller keeps polling instead of passing falsely.
	return true, nil
}

// WaitForResourceGone polls until the named resource of kind no longer
// exists in namespace, or timeout elapses.
func (c *Cluster) WaitForResourceGone(ctx context.Context, kind, namespace, name string, timeout time.Duration) error {
	return pollUntil(ctx, timeout, 5*time.Second,
		func() (bool, error) {
			exists, err := c.ResourceExists(ctx, kind, namespace, name)
			return !exists, err
		},
		func() error {
			out, _ := c.Kubectl(ctx, "-n", namespace, "get", kind, name, "-o", "yaml")
			return fmt.Errorf("%s %s/%s was not deleted within %s:\n%s", kind, namespace, name, timeout, out)
		})
}

// CheckHTTP200 requests "/" on host through Traefik's websecure entrypoint
// until it answers 200 or timeout elapses.
func (c *Cluster) CheckHTTP200(ctx context.Context, host string, timeout time.Duration) error {
	return c.pollGET(ctx, host, "/", timeout, func(resp *http.Response) (ok, retry bool, message string) {
		if resp.StatusCode != http.StatusOK {
			return false, true, resp.Status
		}
		return true, false, resp.Status
	})
}

// CheckRolloutServes restarts deployment and polls "/" on host through
// Traefik until the rollout is done and the old Pod has stopped. Any answer
// other than 200, such as a 502 from routing to a stopping Pod, fails it.
func (c *Cluster) CheckRolloutServes(ctx context.Context, namespace, deployment, host string) error {
	client := &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // kind has no real certificate to check
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	target := fmt.Sprintf("https://127.0.0.1:%d/", c.HTTPSPort)
	stop := make(chan struct{})
	type result struct {
		requests int
		failures []string
	}
	done := make(chan result)
	go func() {
		var r result
		for {
			select {
			case <-stop:
				done <- r
				return
			default:
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				r.failures = append(r.failures, err.Error())
				continue
			}
			req.Host = host
			r.requests++
			resp, err := client.Do(req)
			switch {
			case err != nil:
				r.failures = append(r.failures, err.Error())
			case resp.StatusCode != http.StatusOK:
				r.failures = append(r.failures, resp.Status)
			}
			if resp != nil {
				resp.Body.Close()
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	out, err := c.Kubectl(ctx, "-n", namespace, "rollout", "restart", "deployment/"+deployment)
	if err == nil {
		out, err = c.Kubectl(ctx, "-n", namespace, "rollout", "status", "deployment/"+deployment, "--timeout=3m")
	}
	if err == nil {
		// The old Pod keeps its endpoint until its preStop sleep ends and it
		// stops; keep polling through that.
		err = sleep(ctx, 45*time.Second)
	}
	close(stop)
	r := <-done
	if err != nil {
		return fmt.Errorf("rollout of %s/%s: %w\n%s", namespace, deployment, err, out)
	}
	if len(r.failures) > 0 {
		return fmt.Errorf("GET %s (Host: %s) during a rollout of %s/%s: %d of %d requests failed, first: %s", target, host, namespace, deployment, len(r.failures), r.requests, r.failures[0])
	}
	c.Log("GET %s (Host: %s): %d requests during a rollout of %s/%s, all 200", target, host, r.requests, namespace, deployment)
	return nil
}

// SignIn is the redirect an unauthenticated request to a login-protected
// host must get: straight to the identity provider's authorize endpoint,
// with no oauth2-proxy page in between.
type SignIn struct {
	// LoginURL is the authorize endpoint the Location must lead to, query
	// aside.
	LoginURL string
	// Callback is the redirect_uri: the Platform's auth address, whichever
	// host was requested.
	Callback string
	// CookieDomain is the CSRF cookie's Domain, wide enough for the
	// callback on the auth address to read it.
	CookieDomain string
	// CSRFCookie is oauth2-proxy's cookie-name with _csrf appended.
	CSRFCookie string
}

// CheckSignInRedirect requests path on host through Traefik and requires
// the redirect want describes, with the original https URL to return to in
// the OAuth state. A 3xx without a Location, which a browser renders
// instead of following, fails it. It retries a 5xx or 404 until timeout;
// any other answer fails at once.
func (c *Cluster) CheckSignInRedirect(ctx context.Context, host, path string, want SignIn, timeout time.Duration) error {
	original := "https://" + host + path
	return c.pollGET(ctx, host, path, timeout, func(resp *http.Response) (ok, retry bool, message string) {
		if resp.StatusCode >= 500 {
			return false, true, resp.Status + " (oauth2-proxy is likely not up yet)"
		}
		if resp.StatusCode == http.StatusNotFound {
			// Traefik has no router for host yet, or has dropped it
			// because a middleware it names does not exist.
			return false, true, resp.Status + " (no Traefik route for the host yet)"
		}
		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			return false, false, resp.Status + ", want a redirect (3xx)"
		}
		location := resp.Header.Get("Location")
		target, err := url.Parse(location)
		if err != nil || location == "" {
			return false, false, fmt.Sprintf("%s with Location %q, want a redirect to %s", resp.Status, location, want.LoginURL)
		}
		query := target.Query()
		target.RawQuery = ""
		if target.String() != want.LoginURL {
			return false, false, fmt.Sprintf("%s -> %s, want a redirect to %s", resp.Status, location, want.LoginURL)
		}
		if got := query.Get("redirect_uri"); got != want.Callback {
			return false, false, fmt.Sprintf("%s -> %s: redirect_uri %q, want %q", resp.Status, location, got, want.Callback)
		}
		// oauth2-proxy's state is "<csrf hash>:<URL to return to>".
		if state := query.Get("state"); !strings.HasSuffix(state, ":"+original) {
			return false, false, fmt.Sprintf("%s -> %s: state %q, want it to end with :%s", resp.Status, location, state, original)
		}
		csrf := false
		for _, cookie := range resp.Cookies() {
			if cookie.Name == want.CSRFCookie && cookie.Domain == want.CookieDomain {
				csrf = true
			}
		}
		if !csrf {
			return false, false, fmt.Sprintf("%s -> %s: no %s cookie for %s in %q", resp.Status, location, want.CSRFCookie, want.CookieDomain, resp.Header.Values("Set-Cookie"))
		}
		return true, false, resp.Status + " -> " + location
	})
}

// pollGET GETs path on host through Traefik's websecure entrypoint until
// want accepts the response. Redirects are not followed, and certificates
// are not verified: no DNS-01 issuer can complete inside kind, so Traefik
// serves its default certificate. want's retry result says whether polling
// again could still help; when it cannot, the check fails at once.
// Connection failures are always retried.
func (c *Cluster) pollGET(ctx context.Context, host, path string, timeout time.Duration, want func(resp *http.Response) (ok, retry bool, message string)) error {
	return c.pollGETAt(ctx, fmt.Sprintf("https://127.0.0.1:%d%s", c.HTTPSPort, path), host, timeout, want)
}

// pollGETAt is pollGET for a full URL, so a check can use the plain HTTP
// entrypoint too.
func (c *Cluster) pollGETAt(ctx context.Context, url, host string, timeout time.Duration, want func(resp *http.Response) (ok, retry bool, message string)) error {
	client := &http.Client{
		Timeout:       10 * time.Second,
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // kind has no real certificate to check
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var lastErr error
	return pollUntil(ctx, timeout, 3*time.Second,
		func() (bool, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return false, err
			}
			req.Host = host
			resp, err := client.Do(req)
			if err != nil {
				lastErr = fmt.Errorf("GET %s (Host: %s): %w", url, host, err)
				return false, nil
			}
			resp.Body.Close()
			ok, retry, message := want(resp)
			if !ok {
				err := fmt.Errorf("GET %s (Host: %s): %s", url, host, message)
				if retry {
					lastErr = err
					return false, nil
				}
				return false, err
			}
			c.Log("GET %s (Host: %s): %s", url, host, message)
			return true, nil
		},
		func() error { return lastErr })
}

// CheckACMEChallengeBypassesLogin proves that cert-manager's HTTP-01
// challenge for a login-protected custom domain is not sent to sign-in. kind
// can run no real challenge, so it creates an Ingress shaped like the one
// cert-manager v1.21 creates for a solver (pkg/issuer/acme/http/ingress.go),
// backed by service:port, and deletes it afterwards.
//
// Through Traefik, https://host/<challenge path> must then reach that
// backend (an nginx 404) rather than the login middleware's redirect, and
// http://host/<challenge path> must redirect to the same https URL, which
// Let's Encrypt's validator follows.
func (c *Cluster) CheckACMEChallengeBypassesLogin(ctx context.Context, namespace, host, service string, port int, timeout time.Duration) error {
	const name = "cm-acme-http-solver-e2e"
	path := "/.well-known/acme-challenge/e2e-token"
	manifest := fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    acme.cert-manager.io/http01-solver: "true"
  annotations:
    nginx.ingress.kubernetes.io/whitelist-source-range: 0.0.0.0/0,::/0
spec:
  ingressClassName: traefik
  rules:
    - host: %[3]s
      http:
        paths:
          - path: %[4]s
            pathType: Exact
            backend:
              service:
                name: %[5]s
                port:
                  number: %[6]d
`, name, namespace, host, path, service, port)
	if err := c.Apply(ctx, manifest); err != nil {
		return err
	}
	defer func() {
		if out, err := c.Kubectl(context.WithoutCancel(ctx), "-n", namespace, "delete", "ingress", name, "--ignore-not-found"); err != nil {
			c.Log("deleting ingress %s/%s: %v\n%s", namespace, name, err, out)
		}
	}()
	return c.checkACMEChallengeServed(ctx, host, path, timeout)
}

func (c *Cluster) checkACMEChallengeServed(ctx context.Context, host, path string, timeout time.Duration) error {
	err := c.pollGET(ctx, host, path, timeout, func(resp *http.Response) (ok, retry bool, message string) {
		server := resp.Header.Get("Server")
		switch {
		case resp.StatusCode == http.StatusNotFound && strings.HasPrefix(server, "nginx"):
			return true, false, resp.Status + " from " + server + " (the challenge's own backend)"
		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			return false, true, fmt.Sprintf("%s -> %s: the challenge path went through the login middleware", resp.Status, resp.Header.Get("Location"))
		default:
			return false, true, fmt.Sprintf("%s (Server %q), want the challenge's own backend, an nginx 404", resp.Status, server)
		}
	})
	if err != nil {
		return err
	}

	// The Location's port is whatever the chart publishes, so it is not
	// checked.
	return c.pollGETAt(ctx, fmt.Sprintf("http://127.0.0.1:%d%s", c.HTTPPort, path), host, timeout, func(resp *http.Response) (ok, retry bool, message string) {
		location := resp.Header.Get("Location")
		target, err := url.Parse(location)
		if resp.StatusCode < 300 || resp.StatusCode >= 400 || err != nil || target.Scheme != "https" || target.Hostname() != host || target.Path != path {
			return false, false, fmt.Sprintf("%s -> %q, want a redirect to https://%s%s", resp.Status, location, host, path)
		}
		return true, false, resp.Status + " -> " + location
	})
}

// pollUntil calls attempt every interval until it reports done, an error, or
// timeout elapses, in which case onTimeout builds the error returned.
func pollUntil(ctx context.Context, timeout, interval time.Duration, attempt func() (done bool, err error), onTimeout func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		done, err := attempt()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return onTimeout()
		}
		if err := sleep(ctx, interval); err != nil {
			return err
		}
	}
}

// Repository is served as <GitBaseURL>/<Name>.git with one commit on main.
// Files maps a path inside the repository ("." for the root) to a local file
// or directory copied there.
type Repository struct {
	Name  string
	Files map[string]string
}

// ServeGitRepositories packs the repositories into a ConfigMap, runs the git
// server on it, and checks from the host that git can list each one.
func (c *Cluster) ServeGitRepositories(ctx context.Context, repos ...Repository) error {
	c.Log("building the git server image with %s", c.Provider)
	out, err := c.run(ctx, c.Provider, "build", "-t", gitServerImage, filepath.Join(c.RepoRoot, "test", "e2e", "gitserver"))
	if err != nil {
		return fmt.Errorf("%s build: %w\n%s", c.Provider, err, out)
	}
	if err := c.loadImage(ctx, gitServerImage); err != nil {
		return err
	}

	work, err := os.MkdirTemp("", "iidp-e2e-git-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	bare := filepath.Join(work, "repositories")
	if err := os.Mkdir(bare, 0o755); err != nil {
		return err
	}
	for _, repo := range repos {
		if err := c.buildRepository(ctx, work, bare, repo); err != nil {
			return err
		}
	}
	archive, err := tarGz(bare)
	if err != nil {
		return err
	}
	archiveFile, err := writeTemp("iidp-e2e-repos-*.tar.gz", archive)
	if err != nil {
		return err
	}
	defer os.Remove(archiveFile)
	sum := sha256.Sum256(archive)
	checksum := hex.EncodeToString(sum[:])

	if err := c.Apply(ctx, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: "+GitServerNamespace+"\n"); err != nil {
		return err
	}
	configMap, err := c.Kubectl(ctx, "-n", GitServerNamespace, "create", "configmap", "git-repositories",
		"--from-file=repos.tar.gz="+archiveFile, "--dry-run=client", "-o", "yaml")
	if err != nil {
		return fmt.Errorf("render git-repositories ConfigMap: %w\n%s", err, configMap)
	}
	// Server-side: client-side apply copies the object into the
	// last-applied-configuration annotation, capped at 256 KiB, which the
	// packed repositories outgrew. A ConfigMap itself may hold 1 MiB.
	if err := c.Apply(ctx, configMap, "--server-side", "--force-conflicts"); err != nil {
		return err
	}
	if err := c.Apply(ctx, gitServerManifest(checksum)); err != nil {
		return err
	}
	if out, err := c.Kubectl(ctx, "-n", GitServerNamespace, "rollout", "status", "deployment/git-server", "--timeout=3m"); err != nil {
		return fmt.Errorf("wait for git server: %w\n%s", err, out)
	}
	for _, repo := range repos {
		if err := c.checkRepository(ctx, repo.Name); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cluster) buildRepository(ctx context.Context, work, bare string, repo Repository) error {
	bareDir := filepath.Join(bare, repo.Name+".git")
	workDir := filepath.Join(work, repo.Name+"-work")
	if out, err := c.run(ctx, "git", "init", "-q", "--bare", "-b", "main", bareDir); err != nil {
		return fmt.Errorf("git init --bare: %w\n%s", err, out)
	}
	// git-http-backend refuses pushes by default; PushToRepository needs
	// them. The setting travels in the packed repository's config.
	if out, err := c.run(ctx, "git", "-C", bareDir, "config", "--bool", "http.receivepack", "true"); err != nil {
		return fmt.Errorf("git config http.receivepack: %w\n%s", err, out)
	}
	if out, err := c.run(ctx, "git", "init", "-q", "-b", "main", workDir); err != nil {
		return fmt.Errorf("git init: %w\n%s", err, out)
	}
	paths := make([]string, 0, len(repo.Files))
	for path := range repo.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := copyTree(repo.Files[path], filepath.Join(workDir, path)); err != nil {
			return fmt.Errorf("copy %s into %s: %w", repo.Files[path], repo.Name, err)
		}
	}
	git := func(args ...string) error {
		args = append([]string{"-C", workDir,
			"-c", "user.name=iidp e2e", "-c", "user.email=e2e@iidp.invalid",
			"-c", "commit.gpgsign=false"}, args...)
		out, err := c.run(ctx, "git", args...)
		if err != nil {
			return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
		}
		return nil
	}
	if err := git("add", "-A"); err != nil {
		return err
	}
	if err := git("commit", "-q", "-m", "Fixture for the kind harness"); err != nil {
		return err
	}
	if err := git("push", "-q", bareDir, "HEAD:main"); err != nil {
		return err
	}
	c.Log("packed repository %s.git", repo.Name)
	return nil
}

// withGitServerPortForward runs fn with the named repository's URL as seen
// from the host through a port-forward to the git server.
func (c *Cluster) withGitServerPortForward(ctx context.Context, name string, fn func(ctx context.Context, url string) error) error {
	return c.withPortForward(ctx, "git-server", func(ctx context.Context, base string) error {
		return fn(ctx, base+"/git/"+name+".git")
	})
}

// withPortForward port-forwards a free local port to port 80 of service in
// GitServerNamespace for the duration of fn, which receives its base URL
// as seen from the host (http://127.0.0.1:<port>).
func (c *Cluster) withPortForward(ctx context.Context, service string, fn func(ctx context.Context, base string) error) error {
	port, err := freePort()
	if err != nil {
		return err
	}
	forwardCtx, cancel := context.WithCancel(ctx)
	forward := exec.CommandContext(forwardCtx, "kubectl", "-n", GitServerNamespace, "port-forward", "service/"+service, fmt.Sprintf("%d:80", port))
	forward.Env = c.env()
	if err := forward.Start(); err != nil {
		cancel()
		return fmt.Errorf("kubectl port-forward: %w", err)
	}
	// port-forward only exits when its context is cancelled, so cancel
	// before waiting.
	defer func() {
		cancel()
		forward.Wait()
	}()
	// Start returning does not mean the local port is listening yet.
	if err := waitForLocalPort(ctx, port, 10*time.Second); err != nil {
		return fmt.Errorf("kubectl port-forward to %s never started listening on 127.0.0.1:%d: %w", service, port, err)
	}
	return fn(ctx, fmt.Sprintf("http://127.0.0.1:%d", port))
}

func waitForLocalPort(ctx context.Context, port int, timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	return pollUntil(ctx, timeout, 200*time.Millisecond,
		func() (bool, error) {
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				return false, nil
			}
			conn.Close()
			return true, nil
		},
		func() error { return fmt.Errorf("no listener on %s within %s", addr, timeout) })
}

// checkRepository lists the repository with the host's git, so a broken
// server fails here clearly rather than as an ArgoCD condition later.
func (c *Cluster) checkRepository(ctx context.Context, name string) error {
	return c.withGitServerPortForward(ctx, name, func(ctx context.Context, url string) error {
		var lastErr error
		for attempt := 0; attempt < 30; attempt++ {
			out, err := c.run(ctx, "git", "ls-remote", "--heads", url)
			if err == nil && strings.Contains(out, "refs/heads/main") {
				c.Log("git ls-remote %s: %s", url, strings.TrimSpace(out))
				return nil
			}
			lastErr = fmt.Errorf("git ls-remote %s: %v\n%s", url, err, out)
			if sleep(ctx, time.Second) != nil {
				break
			}
		}
		return lastErr
	})
}

// PushToRepository clones the named served repository, lets mutate change
// the clone at dir, and pushes a commit to main if mutate reports a change,
// as iidp app delete does to a real Platform repository. ArgoCD picks the
// commit up on its next reconcile.
func (c *Cluster) PushToRepository(ctx context.Context, name, commitMessage string, mutate func(dir string) (changed bool, err error)) error {
	return c.withGitServerPortForward(ctx, name, func(ctx context.Context, url string) error {
		work, err := os.MkdirTemp("", "iidp-e2e-push-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(work)
		dir := filepath.Join(work, "clone")
		if out, err := c.run(ctx, "git", "clone", "-q", "--branch", "main", url, dir); err != nil {
			return fmt.Errorf("git clone %s: %w\n%s", url, err, out)
		}
		changed, err := mutate(dir)
		if err != nil {
			return fmt.Errorf("mutate %s: %w", name, err)
		}
		if !changed {
			c.Log("PushToRepository %s: mutate reported no change; nothing pushed", name)
			return nil
		}
		git := func(args ...string) (string, error) {
			args = append([]string{"-C", dir,
				"-c", "user.name=iidp e2e", "-c", "user.email=e2e@iidp.invalid",
				"-c", "commit.gpgsign=false"}, args...)
			out, err := c.run(ctx, "git", args...)
			if err != nil {
				return out, fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
			}
			return out, nil
		}
		if _, err := git("add", "-A"); err != nil {
			return err
		}
		if _, err := git("commit", "-q", "-m", commitMessage); err != nil {
			return err
		}
		if out, err := git("push", "-q", "origin", "HEAD:main"); err != nil {
			return fmt.Errorf("push to %s failed (is http.receivepack enabled on the served repository?): %w\n%s", name, err, out)
		}
		sha, _ := git("rev-parse", "HEAD")
		c.Log("PushToRepository %s: pushed %q as %s", name, commitMessage, strings.TrimSpace(sha))
		return nil
	})
}

// ApplyRootApplication applies the root Application `platform` as
// cloud-init does.
func (c *Cluster) ApplyRootApplication(ctx context.Context, repoURL, path string) error {
	c.Log("applying the root Application platform -> %s %s", repoURL, path)
	return c.Apply(ctx, fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: platform
  namespace: argocd
spec:
  project: default
  source:
    repoURL: %s
    targetRevision: HEAD
    path: %s
  destination:
    server: https://kubernetes.default.svc
    namespace: argocd
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
      - ServerSideApply=true
`, repoURL, path))
}

// ApplicationStatus is what the harness reads from an ArgoCD Application.
type ApplicationStatus struct {
	Name    string
	Sync    string // Synced, OutOfSync, Unknown
	Health  string // Healthy, Progressing, Degraded, Missing, Suspended, Unknown
	Phase   string // last operation phase
	Message string // last operation message
	// Conditions are "<type>: <message>".
	Conditions []string
}

// Applications lists the ArgoCD Applications in the argocd namespace.
func (c *Cluster) Applications(ctx context.Context) (map[string]ApplicationStatus, error) {
	out, err := c.Kubectl(ctx, "-n", "argocd", "get", "applications.argoproj.io", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("list Applications: %w\n%s", err, out)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Sync struct {
					Status string `json:"status"`
				} `json:"sync"`
				Health struct {
					Status string `json:"status"`
				} `json:"health"`
				Conditions []struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"conditions"`
				OperationState struct {
					Phase   string `json:"phase"`
					Message string `json:"message"`
				} `json:"operationState"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parse Applications: %w", err)
	}
	apps := map[string]ApplicationStatus{}
	for _, item := range list.Items {
		status := ApplicationStatus{
			Name:    item.Metadata.Name,
			Sync:    item.Status.Sync.Status,
			Health:  item.Status.Health.Status,
			Phase:   item.Status.OperationState.Phase,
			Message: item.Status.OperationState.Message,
		}
		for _, condition := range item.Status.Conditions {
			status.Conditions = append(status.Conditions, condition.Type+": "+condition.Message)
		}
		apps[status.Name] = status
	}
	return apps, nil
}

// Expectation is what an Application must reach.
type Expectation int

const (
	// Synced: the Application exists and its sync status is Synced.
	Synced Expectation = iota
	// Healthy: Synced and health status Healthy.
	Healthy
)

func (e Expectation) String() string {
	if e == Healthy {
		return "Synced+Healthy"
	}
	return "Synced"
}

// Met reports whether the status satisfies the expectation.
func (e Expectation) Met(s ApplicationStatus) bool {
	if s.Sync != "Synced" {
		return false
	}
	return e == Synced || s.Health == "Healthy"
}

// WaitForApplications polls until every named Application meets its
// expectation, logging each change and a summary every 30 seconds. On
// timeout it logs diagnostics and returns an error naming what was unmet.
func (c *Cluster) WaitForApplications(ctx context.Context, want map[string]Expectation, timeout time.Duration) error {
	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)
	c.Log("waiting up to %s for %d Applications: %s", timeout, len(names), strings.Join(names, ", "))

	deadline := time.Now().Add(timeout)
	start := time.Now()
	lastSeen := map[string]string{}
	lastSummary := time.Time{}
	for {
		apps, err := c.Applications(ctx)
		if err != nil {
			return err
		}
		var unmet []string
		for _, name := range names {
			app, ok := apps[name]
			line := "absent"
			if ok {
				line = fmt.Sprintf("sync=%s health=%s", orUnknown(app.Sync), orUnknown(app.Health))
				if app.Phase != "" && app.Phase != "Succeeded" {
					line += " operation=" + app.Phase
				}
			}
			if lastSeen[name] != line {
				c.Log("[%4.0fs] %-20s %s", time.Since(start).Seconds(), name, line)
				lastSeen[name] = line
			}
			if !ok || !want[name].Met(app) {
				unmet = append(unmet, name)
			}
		}
		if len(unmet) == 0 {
			c.Log("all %d Applications reached their expected state after %s", len(names), time.Since(start).Round(time.Second))
			// Logged on green runs too, so a shrinking CPU margin on the
			// 2 vCPU CI runner shows before it starts failing.
			c.logNodeAllocatedResources(ctx)
			return nil
		}
		if time.Since(lastSummary) >= 30*time.Second {
			c.Log("[%4.0fs] still waiting for: %s", time.Since(start).Seconds(), strings.Join(unmet, ", "))
			lastSummary = time.Now()
		}
		if time.Now().After(deadline) {
			c.DumpDiagnostics(ctx, apps, unmet)
			return fmt.Errorf("after %s these Applications had not reached %s: %s", timeout, expectations(want, unmet), strings.Join(unmet, ", "))
		}
		if err := sleep(ctx, 10*time.Second); err != nil {
			return err
		}
	}
}

// logNodeAllocatedResources logs the node's "Allocated resources:" table.
// Best-effort: errors are ignored.
func (c *Cluster) logNodeAllocatedResources(ctx context.Context) {
	out, err := c.Kubectl(ctx, "describe", "node")
	if err != nil {
		return
	}
	if i := strings.Index(out, "Allocated resources:"); i >= 0 {
		c.Log("node allocated resources:\n%s", out[i:])
	}
}

// DumpDiagnostics logs enough about the named Applications, ArgoCD and the
// cluster to diagnose a failed run, including the argocd self-takeover race,
// without a kept cluster.
func (c *Cluster) DumpDiagnostics(ctx context.Context, apps map[string]ApplicationStatus, names []string) {
	for _, name := range names {
		app, ok := apps[name]
		if !ok {
			c.Log("Application %s: not found", name)
			continue
		}
		c.Log("Application %s: sync=%s health=%s operation=%s %s", name, orUnknown(app.Sync), orUnknown(app.Health), app.Phase, truncate(app.Message, 600))
		for _, condition := range app.Conditions {
			c.Log("  condition %s", truncate(condition, 600))
		}
	}
	if out, err := c.Kubectl(ctx, "get", "pods", "-A", "--field-selector=status.phase!=Running,status.phase!=Succeeded"); err == nil {
		c.Log("pods not running:\n%s", out)
	}
	// Ages and restart counts show whether the application controller is
	// mid-replacement, which is the takeover race.
	if out, err := c.Kubectl(ctx, "-n", "argocd", "get", "pods", "-o", "wide"); err == nil {
		c.Log("argocd namespace pods:\n%s", out)
	} else {
		c.Log("argocd namespace pods: kubectl get failed: %v\n%s", err, out)
	}
	// A Pending Pod is usually short of CPU or memory on kind's single node;
	// these say which pod, and by how much.
	if out, err := c.Kubectl(ctx, "get", "events", "-A", "--field-selector=reason=FailedScheduling", "--sort-by=.lastTimestamp"); err == nil && strings.TrimSpace(out) != "" {
		c.Log("FailedScheduling events:\n%s", out)
	}
	if out, err := c.Kubectl(ctx, "describe", "node"); err == nil {
		if i := strings.Index(out, "Non-terminated Pods:"); i >= 0 {
			c.Log("node pods and allocated resources:\n%s", out[i:])
		}
	}
	if out, err := c.Kubectl(ctx, "-n", "argocd", "logs", "deployment/argocd-repo-server", "--tail=40"); err == nil {
		c.Log("argocd-repo-server log tail:\n%s", out)
	} else {
		c.Log("argocd-repo-server log tail: kubectl logs failed: %v\n%s", err, out)
	}
	// The error is logged too: it fails exactly when the controller has
	// been deleted and not yet recreated.
	if out, err := c.Kubectl(ctx, "-n", "argocd", "logs", "statefulset/argocd-application-controller", "--tail=80"); err == nil {
		c.Log("argocd-application-controller log tail:\n%s", out)
	} else {
		c.Log("argocd-application-controller log tail: kubectl logs failed: %v\n%s", err, out)
	}
	// The redis-secret-init PreSync hook is where the takeover has stalled
	// before: its status and log tell a slow Job from a controller that
	// stopped reporting it.
	if out, err := c.Kubectl(ctx, "-n", "argocd", "get", "job", "argocd-redis-secret-init", "-o", "yaml"); err == nil {
		c.Log("argocd-redis-secret-init Job:\n%s", out)
	} else {
		c.Log("argocd-redis-secret-init Job: kubectl get failed: %v\n%s", err, out)
	}
	if out, err := c.Kubectl(ctx, "-n", "argocd", "logs", "-l", "job-name=argocd-redis-secret-init", "--tail=40", "--all-containers"); err == nil {
		c.Log("argocd-redis-secret-init pod log tail:\n%s", out)
	} else {
		c.Log("argocd-redis-secret-init pod log tail: kubectl logs failed: %v\n%s", err, out)
	}
	if out, err := c.Kubectl(ctx, "-n", "argocd", "describe", "application", "argocd"); err == nil {
		c.Log("argocd Application describe:\n%s", out)
	}
	if out, err := c.Kubectl(ctx, "get", "events", "-A", "--sort-by=.lastTimestamp"); err == nil {
		c.Log("recent events:\n%s", tailString(out, 6000))
	}
}

func expectations(want map[string]Expectation, names []string) string {
	seen := map[string]bool{}
	var kinds []string
	for _, name := range names {
		s := want[name].String()
		if !seen[s] {
			seen[s] = true
			kinds = append(kinds, s)
		}
	}
	return strings.Join(kinds, " or ")
}

func orUnknown(s string) string {
	if s == "" {
		return "Unknown"
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func tailString(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// gitServerManifest is the git server Deployment and Service. The archive's
// checksum is a pod annotation so that serving new content rolls the pod.
func gitServerManifest(checksum string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: git-server
  namespace: %[1]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: git-server
  template:
    metadata:
      labels:
        app: git-server
      annotations:
        iidp.itema.no/repositories: %[3]s
    spec:
      initContainers:
        - name: unpack
          image: %[2]s
          imagePullPolicy: Never
          command: ["sh", "-c", "tar -xzf /repositories/repos.tar.gz -C /srv/git && chown -R 100:101 /srv/git"]
          securityContext:
            runAsUser: 0
          volumeMounts:
            - name: repositories
              mountPath: /repositories
            - name: git
              mountPath: /srv/git
      containers:
        - name: git-server
          image: %[2]s
          imagePullPolicy: Never
          securityContext:
            runAsUser: 100
            runAsGroup: 101
          ports:
            - containerPort: 8080
          readinessProbe:
            tcpSocket:
              port: 8080
          volumeMounts:
            - name: git
              mountPath: /srv/git
      volumes:
        - name: repositories
          configMap:
            name: git-repositories
        - name: git
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: git-server
  namespace: %[1]s
spec:
  selector:
    app: git-server
  ports:
    - port: 80
      targetPort: 8080
`, GitServerNamespace, gitServerImage, checksum)
}

// objectStorageManifest is a single versitygw Pod serving its posix backend
// from an emptyDir. That backend treats each top-level directory as a
// bucket, so an init container creates the bucket with mkdir. --region
// us-east-1 is what botocore, under the Barman Cloud plugin, signs with
// when the ObjectStore names none; the gateway rejects any other. CPU
// requests are 10m to leave the CI runner's 2 vCPUs to the fixtures.
func objectStorageManifest() string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: object-storage
  namespace: %[1]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: object-storage
  template:
    metadata:
      labels:
        app: object-storage
    spec:
      initContainers:
        - name: create-bucket
          image: %[2]s
          command: ["mkdir", "-p", "/data/%[5]s"]
          resources:
            requests:
              cpu: "10m"
              memory: "16Mi"
            limits:
              memory: "32Mi"
          volumeMounts:
            - name: data
              mountPath: /data
      containers:
        - name: versitygw
          image: %[2]s
          args: ["--port", ":7070", "--region", "us-east-1", "posix", "/data"]
          env:
            - name: ROOT_ACCESS_KEY
              value: %[3]s
            - name: ROOT_SECRET_KEY
              value: %[4]s
          ports:
            - containerPort: 7070
          readinessProbe:
            tcpSocket:
              port: 7070
            initialDelaySeconds: 1
            periodSeconds: 2
          resources:
            requests:
              cpu: "10m"
              memory: "32Mi"
            limits:
              cpu: "250m"
              memory: "256Mi"
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: object-storage
  namespace: %[1]s
spec:
  selector:
    app: object-storage
  ports:
    - port: 7070
      targetPort: 7070
`, GitServerNamespace, objectStorageImage, ObjectStorageAccessKey, ObjectStorageSecretKey, ObjectStorageBucket)
}

func (c *Cluster) env() []string {
	return append(os.Environ(), "KUBECONFIG="+c.Kubeconfig)
}

func (c *Cluster) run(ctx context.Context, name string, args ...string) (string, error) {
	return c.runWithStdin(ctx, nil, name, args...)
}

func (c *Cluster) runWithStdin(ctx context.Context, stdin io.Reader, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = c.env()
	cmd.Stdin = stdin
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func writeTemp(pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// copyTree copies a file or directory to dst, skipping .git directories.
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst, info.Mode())
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" && rel != "." {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(path, target, info.Mode())
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode.Perm())
}

// tarGz archives a directory's contents (paths relative to it).
func tarGz(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			header.Name += "/"
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
