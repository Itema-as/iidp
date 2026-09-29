package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Images from rate-limited registries (docs/implementation-notes/
// 128-e2e-image-cache.md). GitHub-hosted runners share their addresses, so
// Docker Hub's and ECR Public's anonymous pull limits are spent by strangers
// too, and a refused pull fails the whole run: ArgoCD's Redis from ECR
// Public did, three times in two days (#128). PreloadImages pulls each of
// these once on the machine running the test, retrying a refusal, keeps the
// archive in a directory CI caches between runs, and loads it into the
// node, so the node finds the image already present instead of pulling it.

// ImageCacheEnv names the directory PreloadImages keeps its archives in.
// CI sets it and caches the directory; unset, the archives go to a
// temporary directory removed after the run.
const ImageCacheEnv = "IIDP_E2E_IMAGE_CACHE"

// imageCacheIndex is the file in the cache directory listing the archives
// of the last run, one per line. CI keys the cache on its hash.
const imageCacheIndex = "images.txt"

// rateLimitedRegistries are the registries whose images PreloadImages
// covers. ghcr.io, quay.io and registry.k8s.io have not refused the kind
// job and are left to the node.
var rateLimitedRegistries = map[string]bool{
	"docker.io":          true,
	"ecr-public.aws.com": true,
	"public.ecr.aws":     true,
}

// RegistryImages lists the images from rate-limited registries that the
// Platform components pull in kind: those the rendered argo-cd and Traefik
// charts name, the KSOPS init container's, and Alloy's, all at the versions
// bootstrap/versions.yaml pins. Reading them from the charts means a
// version bump needs no edit here.
func (c *Cluster) RegistryImages(ctx context.Context) ([]string, error) {
	argocd, err := c.argoCDManifest(ctx)
	if err != nil {
		return nil, err
	}
	traefik, err := c.HelmTemplate(ctx, "traefik", c.Versions.K3s.Traefik.ChartURL,
		"--namespace", "kube-system", "--set-string", "image.tag="+c.Versions.K3s.Traefik.Image)
	if err != nil {
		return nil, fmt.Errorf("helm template traefik: %w", err)
	}
	alloy, err := c.alloyImage(ctx)
	if err != nil {
		return nil, err
	}
	images := append(manifestImages(argocd), manifestImages(traefik)...)
	images = append(images, c.Versions.KSOPS.Image, alloy)
	return RateLimited(images), nil
}

// alloyRepository is where the Alloy Operator takes Alloy from. The
// k8s-monitoring chart pins only the tag.
const alloyRepository = "docker.io/grafana/alloy"

var alloyPinnedTag = regexp.MustCompile(`define "collector\.alloy\.pinnedImageTag" -}}\s*([^{\s]+)\s*{{`)

// alloyImage is the Alloy image the Alloy Operator runs the monitoring
// collectors with. The operator, not the rendered k8s-monitoring chart,
// names it, so it is read from the tag the chart pins for the operator
// (collector.alloy.pinnedImageTag, in
// templates/collectors/_collector_version.tpl).
func (c *Cluster) alloyImage(ctx context.Context) (string, error) {
	m := c.Versions.K8sMonitoring
	dir, err := os.MkdirTemp("", "iidp-e2e-k8s-monitoring-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	if out, err := c.Helm(ctx, "pull", "k8s-monitoring", "--repo", m.Repository, "--version", m.Chart, "--untar", "--untardir", dir); err != nil {
		return "", fmt.Errorf("helm pull k8s-monitoring %s: %w\n%s", m.Chart, err, out)
	}
	tpl, err := os.ReadFile(filepath.Join(dir, "k8s-monitoring", "templates", "collectors", "_collector_version.tpl"))
	if err != nil {
		return "", fmt.Errorf("k8s-monitoring %s no longer pins Alloy's tag where the harness reads it: %w", m.Chart, err)
	}
	match := alloyPinnedTag.FindSubmatch(tpl)
	if match == nil {
		return "", fmt.Errorf("k8s-monitoring %s: no collector.alloy.pinnedImageTag in _collector_version.tpl:\n%s", m.Chart, tpl)
	}
	return alloyRepository + ":" + string(match[1]), nil
}

// RateLimited keeps the images from rate-limited registries, sorted and
// without duplicates.
func RateLimited(images []string) []string {
	seen := map[string]bool{}
	for _, image := range images {
		if rateLimitedRegistries[registry(image)] {
			seen[image] = true
		}
	}
	return sortedKeys(seen)
}

var imageLine = regexp.MustCompile(`(?m)^\s*(?:-\s+)?image:\s*["']?([^"'\s]+)`)

// manifestImages returns every image a rendered manifest names.
func manifestImages(manifest string) []string {
	var images []string
	for _, m := range imageLine.FindAllStringSubmatch(manifest, -1) {
		images = append(images, m[1])
	}
	return images
}

// registry returns the registry host of an image reference, the way
// containerd resolves it: a first path component with a dot or a colon, or
// localhost, is a host; anything else is on Docker Hub.
func registry(image string) string {
	first, _, found := strings.Cut(image, "/")
	if !found || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		return "docker.io"
	}
	if first == "index.docker.io" || first == "registry-1.docker.io" {
		return "docker.io"
	}
	return first
}

// PreloadImages loads each image into the node from the image cache, or
// pulls it on this machine first, retrying a refused pull, and saves it to
// the cache. The cache is left holding only these images, and its index
// lists them.
func (c *Cluster) PreloadImages(ctx context.Context, images []string) error {
	dir := os.Getenv(ImageCacheEnv)
	if dir == "" {
		tmp, err := os.MkdirTemp("", "iidp-e2e-images-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		dir = tmp
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	arch, err := c.nodeArch(ctx)
	if err != nil {
		return err
	}
	keep := map[string]bool{imageCacheIndex: true}
	var index []string
	for _, image := range images {
		name := archiveName(image, arch)
		keep[name] = true
		index = append(index, name)
		if err := c.preloadImage(ctx, image, arch, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !keep[e.Name()] {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(index)
	return os.WriteFile(filepath.Join(dir, imageCacheIndex), []byte(strings.Join(index, "\n")+"\n"), 0o644)
}

func (c *Cluster) preloadImage(ctx context.Context, image, arch, archive string) error {
	if _, err := os.Stat(archive); err == nil {
		out, err := c.run(ctx, "kind", "load", "image-archive", archive, "--name", c.Name)
		if err == nil {
			c.Log("image cache: loaded %s from %s, with no registry pull", image, filepath.Base(archive))
			return nil
		}
		c.Log("image cache: loading %s failed, pulling it again: %v\n%s", filepath.Base(archive), err, out)
		os.Remove(archive)
	}
	attempts, err := retryPull(ctx, pullBackoff, func() (string, error) {
		return c.run(ctx, c.Provider, "pull", "--platform", "linux/"+arch, image)
	}, func(attempt int, wait time.Duration, out string) {
		c.Log("image cache: pulling %s failed (attempt %d), retrying in %s:\n%s", image, attempt, wait, strings.TrimSpace(out))
	})
	if err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	c.Log("image cache: pulled %s from its registry (attempt %d)", image, attempts)
	// docker save writes every platform of a multi-platform image unless
	// told one, and fails on those it did not pull; podman save only ever
	// has the one.
	args := []string{"save", "-o", archive + ".partial"}
	if c.Provider == "docker" {
		args = append(args, "--platform", "linux/"+arch)
	}
	if out, err := c.run(ctx, c.Provider, append(args, image)...); err != nil {
		os.Remove(archive + ".partial")
		return fmt.Errorf("%s save %s: %w\n%s", c.Provider, image, err, out)
	}
	if err := os.Rename(archive+".partial", archive); err != nil {
		return err
	}
	if out, err := c.run(ctx, "kind", "load", "image-archive", archive, "--name", c.Name); err != nil {
		return fmt.Errorf("kind load image-archive %s: %w\n%s", image, err, out)
	}
	return nil
}

// pullBackoff is how long retryPull waits before each retry: about four
// minutes in all, long enough for a per-second or per-minute limit to
// clear, well inside the job's timeout.
var pullBackoff = []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 80 * time.Second}

// retryPull calls pull until it succeeds, waiting backoff[i] before retry
// i+1 when the failure looks transient (transientPullError), and returns
// the number of attempts made. A failure that does not, such as a tag that
// does not exist, is returned at once.
func retryPull(ctx context.Context, backoff []time.Duration, pull func() (string, error), onRetry func(attempt int, wait time.Duration, out string)) (int, error) {
	for attempt := 1; ; attempt++ {
		out, err := pull()
		if err == nil {
			return attempt, nil
		}
		if attempt > len(backoff) || !transientPullError(out) {
			return attempt, fmt.Errorf("%w (attempt %d)\n%s", err, attempt, out)
		}
		wait := backoff[attempt-1]
		onRetry(attempt, wait, out)
		if err := sleep(ctx, wait); err != nil {
			return attempt, fmt.Errorf("%w\n%s", err, out)
		}
	}
}

var transientPull = regexp.MustCompile(`(?i)\b(429|too ?many ?requests|toomanyrequests|50[0-4]|internal server error|bad gateway|service unavailable|gateway timeout|timeout|timed out|connection reset|connection refused|unexpected EOF|temporary failure)\b`)

// transientPullError reports whether a failed pull's output reads like a
// refusal or an outage that a later attempt can get past: a rate limit
// (HTTP 429), a server error (5xx) or a network failure.
func transientPullError(out string) bool {
	return transientPull.MatchString(out)
}

// archiveName is the image's archive file name in the cache: the reference
// with its separators replaced, and the platform, since the archive holds
// only the node's.
func archiveName(image, arch string) string {
	return strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(image) + "_linux-" + arch + ".tar"
}

// nodeArch is the kind node's CPU architecture, as Go and OCI name it.
func (c *Cluster) nodeArch(ctx context.Context) (string, error) {
	arch, err := c.Kubectl(ctx, "get", "nodes", "-o", "jsonpath={.items[0].status.nodeInfo.architecture}")
	arch = strings.TrimSpace(arch)
	if err != nil || arch == "" {
		return "", fmt.Errorf("node architecture: %v\n%s", err, arch)
	}
	return arch, nil
}

// LogImageSources logs, from the kubelet's events, which images the node
// pulled from a registry and which it found already present. Events of a
// namespace that has since been deleted are gone with it. Best-effort, like
// the other diagnostics: it never fails the test.
func (c *Cluster) LogImageSources(ctx context.Context) {
	out, err := c.Kubectl(ctx, "get", "events", "-A", "--field-selector=reason=Pulled", "-o", "json")
	if err != nil {
		c.Log("image sources: kubectl get events failed: %v\n%s", err, out)
		return
	}
	var events struct {
		Items []struct {
			Message string `json:"message"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &events); err != nil {
		c.Log("image sources: parse events: %v", err)
		return
	}
	pulled, present := map[string]bool{}, map[string]bool{}
	for _, e := range events.Items {
		image := quoted(e.Message)
		switch {
		case image == "":
		case strings.HasPrefix(e.Message, "Successfully pulled"):
			pulled[image] = true
		case strings.Contains(e.Message, "already present on machine"):
			present[image] = true
		}
	}
	c.Log("images the node pulled from a registry:\n  %s", strings.Join(sortedKeys(pulled), "\n  "))
	c.Log("images the node found already present:\n  %s", strings.Join(sortedKeys(present), "\n  "))
}

// quoted returns the first double-quoted string in s, or "".
func quoted(s string) string {
	start := strings.IndexByte(s, '"')
	if start < 0 {
		return ""
	}
	end := strings.IndexByte(s[start+1:], '"')
	if end < 0 {
		return ""
	}
	return s[start+1 : start+1+end]
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
