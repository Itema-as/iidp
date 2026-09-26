package e2e

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Itema-as/iidp/internal/appconfig"
)

// The Deploy gate in kind (docs/implementation-notes/60-deploy-gate.md).
// The bootstrap installs the gate like every other Platform component; the
// harness supplies what kind lacks: the gate's image, built from this
// working tree and loaded into the node rather than pulled from GHCR; the
// GitHub App credential cloud-init would have written; and fakegithub
// (test/e2e/testdata/fakegithub), an in-cluster stand-in for both GitHub
// Actions' OIDC issuer and the three GitHub REST calls the gate makes. The
// fixture Platform repository's platform.yaml points the gate at all three.
// The image check is not stood in for: the gate asks Docker Hub itself
// whether brochure's nginx tag exists, the registry the node pulls it
// from.
const (
	deployGateImage = "iidp-e2e.local/deploy-gate:dev"
	fakeGitHubImage = "iidp-e2e.local/fake-github:dev"

	// FakeGitHubURL is fakegithub's in-cluster address: the OIDC issuer the
	// fixture's gate trusts, and (under /api) its GitHub API.
	FakeGitHubURL = "http://fake-github." + GitServerNamespace + ".svc.cluster.local"
	// DeployGateHost is the gate's host under the fixture's baseDomain, and
	// DeployGateAudience the audience its tokens must carry.
	DeployGateHost     = "deploy.app.example.test"
	DeployGateAudience = "https://" + DeployGateHost
	// OrgID is Itema-as's GitHub org id, which the bootstrap pins.
	OrgID = 1230559

	// The App credential's ids, and what fakegithub answers for its bot
	// (test/e2e/testdata/fakegithub).
	fakeAppID          = 1
	fakeInstallationID = 1
	FakeBotIdentity    = "iidp-deploy[bot] <41898282+iidp-deploy[bot]@users.noreply.github.com>"
)

// FakeIssuer signs OIDC tokens with the key fakegithub publishes, so the
// gate in the cluster verifies them exactly as it verifies GitHub's.
type FakeIssuer struct {
	key *rsa.PrivateKey
	kid string
}

// Claims are a valid token's claims for repository (bound by
// repositoryID) pushing to ref, as GitHub Actions issues them: the ids as
// decimal strings.
func (f *FakeIssuer) Claims(repository string, repositoryID int64, ref string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":                 FakeGitHubURL,
		"aud":                 DeployGateAudience,
		"iat":                 now.Unix(),
		"nbf":                 now.Unix(),
		"exp":                 now.Add(5 * time.Minute).Unix(),
		"repository":          repository,
		"repository_id":       fmt.Sprint(repositoryID),
		"repository_owner":    "Itema-as",
		"repository_owner_id": fmt.Sprint(OrgID),
		"ref":                 ref,
		"event_name":          "push",
		"actor":               "e2e-developer",
		"actor_id":            "1000001",
		"sha":                 "0123456789abcdef0123456789abcdef01234567",
		"workflow_ref":        repository + "/.github/workflows/deploy.yaml@" + ref,
	}
}

// Sign makes an RS256 token of claims.
func (f *FakeIssuer) Sign(claims map[string]any) (string, error) {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": f.kid})
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	hashed := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, hashed[:])
	if err != nil {
		return "", err
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// InstallDeployGateStandIns builds the gate's image from this working tree
// and loads it into the node, creates the GitHub App credential Secret
// cloud-init writes on the Platform (argocd/platform-repo-github-app, but
// without the label that would make ArgoCD use it: the harness's git server
// takes no credential) and the GHCR pull token Secret
// (argocd/ghcr-pull-token, with a token that is never used), and runs
// fakegithub with a fresh OIDC signing key
// and App key. It runs before the root Application, so the gate's first
// pod finds everything it needs.
func (c *Cluster) InstallDeployGateStandIns(ctx context.Context) (*FakeIssuer, error) {
	c.Log("building the Deploy gate and fakegithub images with %s", c.Provider)
	if err := c.BuildLocalImage(ctx, deployGateImage, "./cmd/iidp-deploy-gate", "iidp-deploy-gate", filepath.Join("cmd", "iidp-deploy-gate", "Dockerfile")); err != nil {
		return nil, err
	}
	if err := c.BuildLocalImage(ctx, fakeGitHubImage, "./test/e2e/testdata/fakegithub", "fakegithub", filepath.Join("test", "e2e", "testdata", "fakegithub", "Dockerfile")); err != nil {
		return nil, err
	}

	issuerKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	appKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	issuer := &FakeIssuer{key: issuerKey, kid: "iidp-e2e"}
	jwks, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": issuer.kid,
		"n": base64.RawURLEncoding.EncodeToString(issuerKey.PublicKey.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(issuerKey.PublicKey.E)).Bytes()),
	}}})
	if err != nil {
		return nil, err
	}
	appPublic, err := x509.MarshalPKIXPublicKey(&appKey.PublicKey)
	if err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp("", "iidp-e2e-gate-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	files := map[string][]byte{
		"jwks.json":          jwks,
		"app-public-key.pem": pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: appPublic}),
		"app-key.pem":        pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(appKey)}),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(work, name), data, 0o600); err != nil {
			return nil, err
		}
	}

	secret, err := c.Kubectl(ctx, "-n", "argocd", "create", "secret", "generic", "platform-repo-github-app",
		fmt.Sprintf("--from-literal=githubAppID=%d", fakeAppID),
		fmt.Sprintf("--from-literal=githubAppInstallationID=%d", fakeInstallationID),
		"--from-file=githubAppPrivateKey="+filepath.Join(work, "app-key.pem"),
		"--dry-run=client", "-o", "yaml")
	if err != nil {
		return nil, fmt.Errorf("render the App credential Secret: %w\n%s", err, secret)
	}
	if err := c.Apply(ctx, secret); err != nil {
		return nil, err
	}
	// The GHCR pull token cloud-init writes for the gate. The gate needs
	// it to start, but never sends it here: the only images it checks in
	// kind are brochure's, on Docker Hub, which it asks anonymously, and
	// it sends the token to ghcr.io alone
	// (docs/implementation-notes/61-image-check.md).
	pullSecret, err := c.Kubectl(ctx, "-n", "argocd", "create", "secret", "generic", "ghcr-pull-token",
		"--from-literal=username=iidp-e2e", "--from-literal=token=ghp_notarealtoken",
		"--dry-run=client", "-o", "yaml")
	if err != nil {
		return nil, fmt.Errorf("render the GHCR pull token Secret: %w\n%s", err, pullSecret)
	}
	if err := c.Apply(ctx, pullSecret); err != nil {
		return nil, err
	}

	if err := c.CreateNamespace(ctx, GitServerNamespace); err != nil {
		return nil, err
	}
	configMap, err := c.Kubectl(ctx, "-n", GitServerNamespace, "create", "configmap", "fake-github",
		"--from-file=jwks.json="+filepath.Join(work, "jwks.json"),
		"--from-file=app-public-key.pem="+filepath.Join(work, "app-public-key.pem"),
		"--dry-run=client", "-o", "yaml")
	if err != nil {
		return nil, fmt.Errorf("render the fake-github ConfigMap: %w\n%s", err, configMap)
	}
	if err := c.Apply(ctx, configMap); err != nil {
		return nil, err
	}
	if err := c.Apply(ctx, fakeGitHubManifest()); err != nil {
		return nil, err
	}
	if out, err := c.Kubectl(ctx, "-n", GitServerNamespace, "rollout", "status", "deployment/fake-github", "--timeout=2m"); err != nil {
		logs, _ := c.Kubectl(ctx, "-n", GitServerNamespace, "logs", "deployment/fake-github")
		return nil, fmt.Errorf("wait for fake-github: %w\n%s\n%s", err, out, logs)
	}
	return issuer, nil
}

// BuildLocalImage cross-compiles the Go package pkg for the node's
// architecture into <context>/linux/<arch>/<binary>, the layout GoReleaser's
// dockers_v2 gives the same Dockerfiles, builds dockerfile (relative to the
// repository root) with the container engine, and loads the image into the
// node.
func (c *Cluster) BuildLocalImage(ctx context.Context, image, pkg, binary, dockerfile string) error {
	arch, err := c.Kubectl(ctx, "get", "nodes", "-o", "jsonpath={.items[0].status.nodeInfo.architecture}")
	arch = strings.TrimSpace(arch)
	if err != nil || arch == "" {
		return fmt.Errorf("node architecture: %v\n%s", err, arch)
	}
	buildContext, err := os.MkdirTemp("", "iidp-e2e-image-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(buildContext)
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", filepath.Join(buildContext, "linux", arch, binary), pkg)
	build.Dir = c.RepoRoot
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build %s for linux/%s: %w\n%s", pkg, arch, err, out)
	}
	out, err := c.run(ctx, c.Provider, "build", "--platform", "linux/"+arch,
		"-f", filepath.Join(c.RepoRoot, dockerfile), "-t", image, buildContext)
	if err != nil {
		return fmt.Errorf("%s build %s: %w\n%s", c.Provider, image, err, out)
	}
	return c.loadImage(ctx, image)
}

// loadImage loads an image from the container engine into the kind node,
// as an archive rather than with kind load docker-image: the latter looks
// the image up in the engine's store under a name the podman provider does
// not always resolve.
func (c *Cluster) loadImage(ctx context.Context, image string) error {
	archive, err := writeTemp("iidp-e2e-image-*.tar", nil)
	if err != nil {
		return err
	}
	defer os.Remove(archive)
	if out, err := c.run(ctx, c.Provider, "save", "-o", archive, image); err != nil {
		return fmt.Errorf("%s save %s: %w\n%s", c.Provider, image, err, out)
	}
	if out, err := c.run(ctx, "kind", "load", "image-archive", archive, "--name", c.Name); err != nil {
		return fmt.Errorf("kind load image-archive %s: %w\n%s", image, err, out)
	}
	return nil
}

// WaitForDeployGate polls the gate's health check through Traefik until it
// answers, so a call right after it is not refused by a route still being
// set up.
func (c *Cluster) WaitForDeployGate(ctx context.Context, timeout time.Duration) error {
	var last string
	return pollUntil(ctx, timeout, 3*time.Second,
		func() (bool, error) {
			status, body, err := c.requestDeployGate(ctx, http.MethodGet, "/healthz", "", nil)
			if err != nil {
				last = err.Error()
				return false, nil
			}
			last = fmt.Sprintf("HTTP %d: %s", status, body)
			return status == http.StatusOK, nil
		},
		func() error {
			return fmt.Errorf("the Deploy gate at %s did not answer /healthz within %s: %s", DeployGateHost, timeout, last)
		})
}

// CallDeployGate makes the deploy call CI makes, through Traefik on the
// host port, with Host set to the gate's, and returns the status and the
// decoded body.
func (c *Cluster) CallDeployGate(ctx context.Context, token, application, environment, tag string) (int, map[string]any, error) {
	return c.CallDeployGateWithIidpYAML(ctx, token, application, environment, tag, nil, nil)
}

// CallDeployGateWithIidpYAML is CallDeployGate carrying a migration
// command and Scheduled tasks, as iidp ci set-image sends them when the
// deployed commit has an iidp.yaml; nil sends no migration command, and
// no tasks leaves the key out.
func (c *Cluster) CallDeployGateWithIidpYAML(ctx context.Context, token, application, environment, tag string, migrationCommand *string, tasks []appconfig.Task) (int, map[string]any, error) {
	request := map[string]any{"application": application, "environment": environment, "tag": tag}
	if migrationCommand != nil {
		request["migrationCommand"] = *migrationCommand
	}
	if len(tasks) > 0 {
		request["tasks"] = tasks
	}
	body, _ := json.Marshal(request)
	status, data, err := c.requestDeployGate(ctx, http.MethodPost, "/v1/deploy", token, body)
	if err != nil {
		return 0, nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return status, nil, fmt.Errorf("the Deploy gate answered HTTP %d with something other than JSON: %s", status, data)
	}
	return status, decoded, nil
}

func (c *Cluster) requestDeployGate(ctx context.Context, method, path, token string, body []byte) (int, []byte, error) {
	client := &http.Client{
		Timeout:   2 * time.Minute,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // kind has no real certificate to check
	}
	url := fmt.Sprintf("https://127.0.0.1:%d%s", c.HTTPSPort, path)
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Host = DeployGateHost
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s (Host: %s): %w", method, url, DeployGateHost, err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes(), nil
}

// ReadRepository clones the named repository through a port-forward to the
// in-cluster git server and hands fn the clone.
func (c *Cluster) ReadRepository(ctx context.Context, name string, fn func(dir string) error) error {
	return c.withGitServerPortForward(ctx, name, func(ctx context.Context, url string) error {
		work, err := os.MkdirTemp("", "iidp-e2e-read-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(work)
		dir := filepath.Join(work, "clone")
		if out, err := c.run(ctx, "git", "clone", "-q", "--branch", "main", url, dir); err != nil {
			return fmt.Errorf("git clone %s: %w\n%s", url, err, out)
		}
		return fn(dir)
	})
}

// RefreshApplication asks ArgoCD to compare an Application with git now
// rather than at its next poll, the way the UI's Refresh button does.
func (c *Cluster) RefreshApplication(ctx context.Context, name string) error {
	if out, err := c.Kubectl(ctx, "-n", "argocd", "annotate", "application", name, "argocd.argoproj.io/refresh=normal", "--overwrite"); err != nil {
		return fmt.Errorf("refresh %s: %w\n%s", name, err, out)
	}
	return nil
}

// fakeGitHubManifest runs fakegithub in GitServerNamespace, with its key
// set and the App's public key from the fake-github ConfigMap. Its CPU
// request is a few millicores for the same reason as the Object Storage
// stand-in's (objectStorageManifest).
func fakeGitHubManifest() string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: fake-github
  namespace: %[1]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: fake-github
  template:
    metadata:
      labels:
        app: fake-github
    spec:
      containers:
        - name: fake-github
          image: %[2]s
          imagePullPolicy: Never
          env:
            - name: JWKS_FILE
              value: /config/jwks.json
            - name: APP_PUBLIC_KEY_FILE
              value: /config/app-public-key.pem
          ports:
            - containerPort: 8080
          readinessProbe:
            httpGet:
              path: /healthz
              port: 8080
            periodSeconds: 2
          resources:
            requests:
              cpu: "5m"
              memory: "16Mi"
            limits:
              memory: "64Mi"
          volumeMounts:
            - name: config
              mountPath: /config
      volumes:
        - name: config
          configMap:
            name: fake-github
---
apiVersion: v1
kind: Service
metadata:
  name: fake-github
  namespace: %[1]s
spec:
  selector:
    app: fake-github
  ports:
    - port: 80
      targetPort: 8080
`, GitServerNamespace, fakeGitHubImage)
}
