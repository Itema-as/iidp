// Package e2e stands up a kind cluster the way the Platform node is
// bootstrapped: Traefik as k3s ships it, ArgoCD from the pinned upstream
// manifest, the age key Secret, and the root Application `platform`, with
// the Platform repository and this repository's bootstrap served by a git
// server inside the cluster. No cloud account is involved.
//
// TestBootstrap (bootstrap_test.go, build tag e2e) drives it end to end.
// The Cluster type is exported so a later test can deploy a fixture
// Application through the same harness and reach Traefik on the host ports
// it maps.
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

	// Traefik's entrypoints are NodePorts in kind (there is no ServiceLB);
	// the kind config maps them to host ports so a test can curl Traefik
	// with a Host header.
	traefikWebNodePort       = 30080
	traefikWebsecureNodePort = 30443
)

// Versions is the part of bootstrap/versions.yaml the harness needs to
// mimic the node.
type Versions struct {
	ArgoCD struct {
		Manifest string `yaml:"manifest"`
	} `yaml:"argocd"`
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
		"argocd.manifest":         v.ArgoCD.Manifest,
		"k3s.traefik.chart":       v.K3s.Traefik.Chart,
		"k3s.traefik.chartURL":    v.K3s.Traefik.ChartURL,
		"k3s.traefik.crdChartURL": v.K3s.Traefik.CRDChartURL,
		"k3s.traefik.image":       v.K3s.Traefik.Image,
		"k3s.helmController":      v.K3s.HelmController,
		"kind.nodeImage":          v.Kind.NodeImage,
	} {
		if value == "" {
			return v, fmt.Errorf("bootstrap/versions.yaml: %s is empty", name)
		}
	}
	return v, nil
}

// RepoRoot finds the repository root (the directory holding go.mod) from
// the working directory, which go test sets to the package directory.
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
	// Name is the kind cluster name.
	Name string
	// Kubeconfig is the file every kubectl and helm call uses.
	Kubeconfig string
	// HTTPPort and HTTPSPort are the host ports mapped to Traefik's web and
	// websecure entrypoints.
	HTTPPort  int
	HTTPSPort int
	// Provider is the container engine kind runs on and images are built
	// with: podman when KIND_EXPERIMENTAL_PROVIDER says so, else docker.
	Provider string
	// RepoRoot is this repository's root; Versions its bootstrap/versions.yaml.
	RepoRoot string
	Versions Versions
	// Log receives progress lines; tests pass t.Logf.
	Log func(format string, args ...any)
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
	return &Cluster{
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
	config := fmt.Sprintf(`kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: %d
        hostPort: %d
      - containerPort: %d
        hostPort: %d
`, traefikWebNodePort, c.HTTPPort, traefikWebsecureNodePort, c.HTTPSPort)
	configFile, err := writeTemp("iidp-e2e-kind-*.yaml", []byte(config))
	if err != nil {
		return err
	}
	defer os.Remove(configFile)
	c.Log("creating kind cluster %s from %s", c.Name, c.Versions.Kind.NodeImage)
	out, err = c.run(ctx, "kind", "create", "cluster",
		"--name", c.Name,
		"--config", configFile,
		"--kubeconfig", c.Kubeconfig,
		"--image", c.Versions.Kind.NodeImage,
		"--wait", "120s")
	if err != nil {
		return fmt.Errorf("kind create cluster: %w\n%s", err, out)
	}
	// kind's single node stands in for the whole Platform node plus, on a
	// hosted CI runner, the CI machine's own overhead; two CoreDNS replicas
	// (kind's default, for the node it does not have) claim CPU and memory
	// the Platform components and the fixture Application need instead. One
	// replica is still enough DNS for a cluster this small.
	if out, err := c.Kubectl(ctx, "-n", "kube-system", "scale", "deployment/coredns", "--replicas=1"); err != nil {
		return fmt.Errorf("scale coredns: %w\n%s", err, out)
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

// Helm runs helm against the cluster and returns its combined output.
func (c *Cluster) Helm(ctx context.Context, args ...string) (string, error) {
	return c.run(ctx, "helm", args...)
}

// InstallTraefik installs the Traefik chart k3s bundles (its own build of
// the upstream chart, from k3s-charts), with the values k3s and the
// bootstrap's HelmChartConfig set: the image k3s pins, the HTTP to HTTPS redirect on the web entrypoint, and (kind
// has no ServiceLB) NodePorts plus the node IP published on Ingress status
// so ArgoCD sees the Ingress as healthy and external-dns has an address to
// read. It also installs the HelmChartConfig CRD from k3s's helm-controller
// so the bootstrap's HelmChartConfig applies; nothing in kind acts on it.
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

// InstallArgoCD installs ArgoCD exactly as cloud-init does
// (infra/platform/cloud-init/user-data.yaml.tftpl) and waits for it to be
// up.
func (c *Cluster) InstallArgoCD(ctx context.Context) error {
	version := c.Versions.ArgoCD.Manifest
	c.Log("installing ArgoCD %s from the upstream manifest", version)
	if err := c.Apply(ctx, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: argocd\n"); err != nil {
		return err
	}
	manifest := fmt.Sprintf("https://raw.githubusercontent.com/argoproj/argo-cd/%s/manifests/install.yaml", version)
	out, err := c.Kubectl(ctx, "apply", "-n", "argocd", "--server-side", "--force-conflicts", "-f", manifest)
	if err != nil {
		return fmt.Errorf("kubectl apply argocd: %w\n%s", err, out)
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

// CreateAgeKeySecret creates the Secret argocd/sops-age with the age private
// key at keyFile under keys.txt, the layout cloud-init produces and KSOPS
// mounts.
func (c *Cluster) CreateAgeKeySecret(ctx context.Context, keyFile string) error {
	out, err := c.Kubectl(ctx, "-n", "argocd", "create", "secret", "generic", "sops-age", "--from-file=keys.txt="+keyFile)
	if err != nil {
		return fmt.Errorf("create sops-age secret: %w\n%s", err, out)
	}
	return nil
}

// CreateNamespace creates a namespace, doing nothing if it already exists
// (kubectl apply is idempotent): the fixture Application's Environments need
// theirs to exist before their ObjectStore and Cluster reconcile, ahead of
// ArgoCD's own CreateNamespace=true, which would otherwise create it later.
func (c *Cluster) CreateNamespace(ctx context.Context, name string) error {
	return c.Apply(ctx, fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", name))
}

// CreateBackupsCredentialsSecret creates the Secret named
// platform.backupsCredentialsSecret (chart/application's default
// backups-credentials) with dummy Object Storage keys, in the given
// namespace. kind has no Object Storage: this is enough for the chart's
// ObjectStore and CloudNativePG Cluster to render and reach Healthy; the
// scheduled backup itself fails against the unreachable endpoint, which is
// expected (docs/implementation-notes/09-e2e-fixture-application.md).
func (c *Cluster) CreateBackupsCredentialsSecret(ctx context.Context, namespace string) error {
	out, err := c.Kubectl(ctx, "-n", namespace, "create", "secret", "generic", "backups-credentials",
		"--from-literal=ACCESS_KEY_ID=dummy", "--from-literal=ACCESS_SECRET_KEY=dummy")
	if err != nil {
		return fmt.Errorf("create backups-credentials secret in %s: %w\n%s", namespace, err, out)
	}
	return nil
}

// JobSucceeded reports whether the named Job's status.succeeded is at least
// one. A Job that does not exist yet, or has not completed, reports false
// with no error, so a caller can poll it.
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

// CheckHTTP200 requests "/" on host through Traefik's websecure entrypoint,
// mapped to HTTPSPort on the host, with the Host header set to host and
// certificate verification disabled: kind's Traefik falls back to its own
// default certificate because no cloud DNS-01 issuer can complete inside
// kind, so there is no certificate to validate against. It retries until
// timeout.
func (c *Cluster) CheckHTTP200(ctx context.Context, host string, timeout time.Duration) error {
	return c.pollGET(ctx, host, timeout, func(resp *http.Response) (ok, retry bool, message string) {
		if resp.StatusCode != http.StatusOK {
			return false, true, resp.Status
		}
		return true, false, resp.Status
	})
}

// CheckRedirect requests "/" on host through Traefik's websecure entrypoint,
// the same way CheckHTTP200 does, but expects a redirect (a 3xx status)
// instead of following it: it is what an unauthenticated request to a
// login.enabled host should get from the itema-login ForwardAuth middleware
// (a straight 5xx instead would mean oauth2-proxy itself is not up). It
// retries until timeout, and fails if any response is neither the wanted
// redirect nor a plain connection error (a non-3xx, non-5xx status is not
// something retrying will fix).
func (c *Cluster) CheckRedirect(ctx context.Context, host string, timeout time.Duration) error {
	return c.pollGET(ctx, host, timeout, func(resp *http.Response) (ok, retry bool, message string) {
		if resp.StatusCode >= 500 {
			return false, true, resp.Status + " (oauth2-proxy is likely not up yet)"
		}
		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			return false, false, resp.Status + ", want a redirect (3xx)"
		}
		return true, false, resp.Status + " -> " + resp.Header.Get("Location")
	})
}

// pollGET is the shared polling shape CheckHTTP200 and CheckRedirect build
// on: GET "/" on host through Traefik's websecure entrypoint (mapped to
// HTTPSPort on the host), certificate verification disabled (kind's Traefik
// falls back to its own default certificate, since no cloud DNS-01 issuer
// can complete inside kind). Redirects are never followed (CheckHTTP200
// never sees one; CheckRedirect wants to see it directly). want inspects
// the response and reports whether it is the wanted one; when it is not,
// retry says whether polling again could still produce it (an unready
// oauth2-proxy, say) as opposed to a wrong status entirely, which fails the
// check immediately instead of waiting out the full timeout. A failure to
// connect at all is always retried.
func (c *Cluster) pollGET(ctx context.Context, host string, timeout time.Duration, want func(resp *http.Response) (ok, retry bool, message string)) error {
	client := &http.Client{
		Timeout:       10 * time.Second,
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // kind has no real certificate to check
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	url := fmt.Sprintf("https://127.0.0.1:%d/", c.HTTPSPort)
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

// Repository is a git repository the in-cluster server serves as
// <GitBaseURL>/<Name>.git: one commit on branch main with Files, a map from
// path inside the repository ("." for the root) to a local file or
// directory copied there.
type Repository struct {
	Name  string
	Files map[string]string
}

// ServeGitRepositories builds the git server image, loads it into the
// cluster, packs the repositories into a ConfigMap and runs the server,
// then checks from the host that a real git client can list each
// repository through it.
func (c *Cluster) ServeGitRepositories(ctx context.Context, repos ...Repository) error {
	c.Log("building the git server image with %s", c.Provider)
	out, err := c.run(ctx, c.Provider, "build", "-t", gitServerImage, filepath.Join(c.RepoRoot, "test", "e2e", "gitserver"))
	if err != nil {
		return fmt.Errorf("%s build: %w\n%s", c.Provider, err, out)
	}
	// Loaded as an archive rather than with kind load docker-image: the
	// latter looks the image up in the engine's store under a name the
	// podman provider does not always resolve.
	imageArchive, err := writeTemp("iidp-e2e-git-server-*.tar", nil)
	if err != nil {
		return err
	}
	defer os.Remove(imageArchive)
	if out, err := c.run(ctx, c.Provider, "save", "-o", imageArchive, gitServerImage); err != nil {
		return fmt.Errorf("%s save: %w\n%s", c.Provider, err, out)
	}
	if out, err := c.run(ctx, "kind", "load", "image-archive", imageArchive, "--name", c.Name); err != nil {
		return fmt.Errorf("kind load image-archive: %w\n%s", err, out)
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
	if err := c.Apply(ctx, configMap); err != nil {
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

// buildRepository makes a bare repository with one commit holding the
// repository's files.
func (c *Cluster) buildRepository(ctx context.Context, work, bare string, repo Repository) error {
	bareDir := filepath.Join(bare, repo.Name+".git")
	workDir := filepath.Join(work, repo.Name+"-work")
	if out, err := c.run(ctx, "git", "init", "-q", "--bare", "-b", "main", bareDir); err != nil {
		return fmt.Errorf("git init --bare: %w\n%s", err, out)
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

// checkRepository port-forwards to the git server and lists the repository
// with the host's git, so a broken server fails here with a clear message
// rather than as an ArgoCD condition later.
func (c *Cluster) checkRepository(ctx context.Context, name string) error {
	port, err := freePort()
	if err != nil {
		return err
	}
	forwardCtx, cancel := context.WithCancel(ctx)
	forward := exec.CommandContext(forwardCtx, "kubectl", "-n", GitServerNamespace, "port-forward", "service/git-server", fmt.Sprintf("%d:80", port))
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
	url := fmt.Sprintf("http://127.0.0.1:%d/git/%s.git", port, name)
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
}

// ApplyRootApplication applies the root Application `platform` with the
// shape cloud-init uses.
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
	// Conditions are "<type>: <message>" for every status condition.
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

// DumpDiagnostics logs what is known about the named Applications, the
// pods that are not running, the tail of the repo server and application
// controller logs, the argocd Application's own operation state, and
// recent cluster events: everything the takeover race (see
// docs/implementation-notes/04-bootstrap.md, "The takeover race") needs to
// be diagnosed from a single failed run's output, without a kept cluster.
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
	// A Pod stuck Pending is almost always the scheduler refusing it (most
	// often insufficient CPU or memory on kind's single node); the events
	// and the node's allocated-resources table say which.
	if out, err := c.Kubectl(ctx, "get", "events", "-A", "--field-selector=reason=FailedScheduling", "--sort-by=.lastTimestamp"); err == nil && strings.TrimSpace(out) != "" {
		c.Log("FailedScheduling events:\n%s", out)
	}
	if out, err := c.Kubectl(ctx, "describe", "node"); err == nil {
		if i := strings.Index(out, "Allocated resources:"); i >= 0 {
			c.Log("node allocated resources:\n%s", out[i:])
		}
	}
	if out, err := c.Kubectl(ctx, "-n", "argocd", "logs", "deployment/argocd-repo-server", "--tail=40"); err == nil {
		c.Log("argocd-repo-server log tail:\n%s", out)
	}
	// The application controller is what runs every sync, including the
	// argocd Application's own takeover of itself; its log tail and the
	// argocd Application's full operation state are the two things that
	// say whether it stalled waiting on a hook, on another component's
	// health, or on the repo server (the takeover race).
	if out, err := c.Kubectl(ctx, "-n", "argocd", "logs", "statefulset/argocd-application-controller", "--tail=80"); err == nil {
		c.Log("argocd-application-controller log tail:\n%s", out)
	}
	if out, err := c.Kubectl(ctx, "-n", "argocd", "describe", "application", "argocd"); err == nil {
		c.Log("argocd Application describe:\n%s", out)
	}
	if out, err := c.Kubectl(ctx, "get", "events", "-A", "--sort-by=.lastTimestamp"); err == nil {
		// Sorted oldest first: keep the tail, the most recent events, not
		// the head, when there are more than fit comfortably in the log.
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

// tailString keeps the last n characters of s, the opposite of truncate:
// useful for output where the newest, most relevant lines are at the end.
func tailString(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// gitServerManifest is the git server Deployment and Service. The checksum
// of the repositories archive is a pod annotation so that serving new
// content rolls the pod.
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
