package e2e

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// A real rate-limit refusal, as containerd reported it.
const ecrRateLimited = `Failed to pull image "ecr-public.aws.com/docker/library/redis:8.6.4-alpine": failed to pull and unpack image: unexpected status from GET request to https://ecr-public.aws.com/v2/docker/library/redis/manifests/sha256:2cc044fc5a07c9b701f8f1255a309ae9ad7856e694ac03513bf3648c01e40763: 429 Too Many Requests`

func TestTransientPullError(t *testing.T) {
	for _, out := range []string{
		ecrRateLimited,
		"Error response from daemon: toomanyrequests: You have reached your unauthenticated pull rate limit.",
		"Error response from daemon: received unexpected HTTP status: 503 Service Unavailable",
		"Error: initializing source docker://nginx:1.30-alpine: pinging container registry registry-1.docker.io: Get \"https://registry-1.docker.io/v2/\": net/http: TLS handshake timeout",
		"read tcp 10.1.0.4:40000->3.216.34.172:443: read: connection reset by peer",
	} {
		if !transientPullError(out) {
			t.Errorf("transientPullError(%q) = false, want true", out)
		}
	}
	for _, out := range []string{
		"Error response from daemon: manifest for nginx:iidp-e2e-no-such-tag not found: manifest unknown: manifest unknown",
		"Error: initializing source docker://ghcr.io/itema-as/later:1.0: reading manifest 1.0 in ghcr.io/itema-as/later: denied",
		// A digest's hex holds 429 and 503 inside a word, not as a status.
		"manifest unknown: sha256:0429aa5030b1c9e",
	} {
		if transientPullError(out) {
			t.Errorf("transientPullError(%q) = true, want false", out)
		}
	}
}

func TestRetryPullRetriesARateLimit(t *testing.T) {
	backoff := []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}
	calls := 0
	var waits []time.Duration
	attempts, err := retryPull(context.Background(), backoff, func() (string, error) {
		calls++
		if calls < 3 {
			return ecrRateLimited, errors.New("exit status 1")
		}
		return "", nil
	}, func(attempt int, wait time.Duration, out string) {
		waits = append(waits, wait)
	})
	if err != nil || attempts != 3 {
		t.Fatalf("retryPull = %d, %v; want 3 attempts, no error", attempts, err)
	}
	if !slices.Equal(waits, backoff[:2]) {
		t.Errorf("waits = %v, want %v", waits, backoff[:2])
	}
}

func TestRetryPullGivesUp(t *testing.T) {
	backoff := []time.Duration{time.Millisecond, time.Millisecond}
	calls := 0
	_, err := retryPull(context.Background(), backoff, func() (string, error) {
		calls++
		return ecrRateLimited, errors.New("exit status 1")
	}, func(int, time.Duration, string) {})
	if err == nil || calls != 3 {
		t.Errorf("retryPull made %d calls, error %v; want 3 calls and an error", calls, err)
	}

	calls = 0
	_, err = retryPull(context.Background(), backoff, func() (string, error) {
		calls++
		return "manifest unknown", errors.New("exit status 1")
	}, func(int, time.Duration, string) {})
	if err == nil || calls != 1 {
		t.Errorf("retryPull made %d calls on a missing tag, error %v; want 1 call and an error", calls, err)
	}
}

func TestRateLimited(t *testing.T) {
	got := RateLimited([]string{
		"quay.io/argoproj/argocd:v3.5.3",
		"ecr-public.aws.com/docker/library/redis:8.6.4-alpine",
		"ghcr.io/dexidp/dex:v2.45.1",
		"viaductoss/ksops:v4.5.1",
		"nginx:1.30-alpine",
		"docker.io/library/nginx:1.30-alpine",
		"rancher/mirrored-library-traefik:3.7.8",
		"iidp-e2e.local/deploy-gate:dev",
		"localhost/foo:dev",
		"ecr-public.aws.com/docker/library/redis:8.6.4-alpine",
	})
	want := []string{
		"docker.io/library/nginx:1.30-alpine",
		"ecr-public.aws.com/docker/library/redis:8.6.4-alpine",
		"nginx:1.30-alpine",
		"rancher/mirrored-library-traefik:3.7.8",
		"viaductoss/ksops:v4.5.1",
	}
	if !slices.Equal(got, want) {
		t.Errorf("RateLimited = %q, want %q", got, want)
	}
}

func TestManifestImages(t *testing.T) {
	manifest := `
      containers:
        - name: redis
          image: ecr-public.aws.com/docker/library/redis:8.6.4-alpine
      initContainers:
        - image: "viaductoss/ksops:v4.5.1"
          name: install-ksops
      # image: commented/out:1
`
	want := []string{"ecr-public.aws.com/docker/library/redis:8.6.4-alpine", "viaductoss/ksops:v4.5.1"}
	if got := manifestImages(manifest); !slices.Equal(got, want) {
		t.Errorf("manifestImages = %q, want %q", got, want)
	}
}

// Fetches the charts over the network, so it runs only where
// IIDP_REQUIRE_CHART_TOOLS is set.
func TestRegistryImagesFollowThePinnedCharts(t *testing.T) {
	if os.Getenv("IIDP_REQUIRE_CHART_TOOLS") == "" {
		t.Skip("fetches charts over the network; set IIDP_REQUIRE_CHART_TOOLS=1 to run")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatal("helm not on PATH and IIDP_REQUIRE_CHART_TOOLS is set")
	}
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	versions, err := LoadVersions(root)
	if err != nil {
		t.Fatal(err)
	}
	c := &Cluster{RepoRoot: root, Versions: versions, Log: t.Logf}
	images, err := c.RegistryImages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("images from rate-limited registries: %q", images)
	var redis, traefik, alloy bool
	for _, image := range images {
		redis = redis || (registry(image) == "ecr-public.aws.com" && strings.Contains(image, "/redis:"))
		traefik = traefik || strings.Contains(image, "/mirrored-library-traefik:")
		alloy = alloy || strings.HasPrefix(image, alloyRepository+":v")
	}
	if !redis || !traefik || !alloy || !slices.Contains(images, versions.KSOPS.Image) {
		t.Errorf("RegistryImages = %q: want ArgoCD's Redis, Traefik, Alloy and %s", images, versions.KSOPS.Image)
	}
}

func TestLocalNodeImage(t *testing.T) {
	for pinned, want := range map[string]string{
		"kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed": "iidp-e2e.local/kind-node:sha256-099e049362a1526b",
		"kindest/node:v1.36.4":                "iidp-e2e.local/kind-node:v1.36.4",
		"localhost:5000/kindest/node:v1.36.4": "iidp-e2e.local/kind-node:v1.36.4",
	} {
		if got := localNodeImage(pinned); got != want {
			t.Errorf("localNodeImage(%q) = %q, want %q", pinned, got, want)
		}
	}
}
