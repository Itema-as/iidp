package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// GitHub-hosted runners share their addresses, so Docker Hub's and ECR
// Public's anonymous pull limits are spent by strangers too, and a refused
// pull fails the whole run. PreloadImages pulls such images on the host with
// retries, keeps the archives in a directory CI caches, and loads them into
// the node so it never pulls them itself.

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
// Platform components pull in kind. They are read from the pinned charts, so
// a version bump needs no edit here.
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
	images := append(manifestImages(argocd), manifestImages(traefik)...)
	images = append(images, c.Versions.KSOPS.Image)
	// Alloy's tag is read from inside a third-party chart. If a release
	// moves it, the node pulls Alloy itself rather than the run failing;
	// TestRegistryImagesFollowThePinnedCharts is what fails.
	if alloy, err := c.alloyImage(ctx); err != nil {
		c.Log("image cache: leaving Alloy to the node: %v", err)
	} else {
		images = append(images, alloy)
	}
	return RateLimited(images), nil
}

// alloyRepository is where the Alloy Operator takes Alloy from. The
// k8s-monitoring chart pins only the tag.
const alloyRepository = "docker.io/grafana/alloy"

var alloyPinnedTag = regexp.MustCompile(`define "collector\.alloy\.pinnedImageTag" -}}\s*([^{\s]+)\s*{{`)

// alloyImage is the image the Alloy Operator runs the collectors with. The
// operator names it, not the rendered chart, so the tag is read from the
// chart's collector.alloy.pinnedImageTag template.
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

// registry returns the registry host of an image reference as containerd
// resolves it: a first path component with a dot or colon, or localhost, is
// a host; anything else is Docker Hub.
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

type imageCache struct {
	dir       string
	temporary bool
	// used are the archives this run needed; finish keeps only these.
	used []string
}

func openImageCache() (*imageCache, error) {
	if dir := os.Getenv(ImageCacheEnv); dir != "" {
		return &imageCache{dir: dir}, os.MkdirAll(dir, 0o755)
	}
	dir, err := os.MkdirTemp("", "iidp-e2e-images-*")
	return &imageCache{dir: dir, temporary: true}, err
}

func (ic *imageCache) archive(name string) string {
	ic.used = append(ic.used, name)
	return filepath.Join(ic.dir, name)
}

// finish removes the archives this run did not use and writes the index.
func (ic *imageCache) finish() error {
	used := map[string]bool{}
	for _, name := range ic.used {
		used[name] = true
	}
	entries, err := os.ReadDir(ic.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !used[e.Name()] && e.Name() != imageCacheIndex {
			os.RemoveAll(filepath.Join(ic.dir, e.Name()))
		}
	}
	index := sortedKeys(used)
	return os.WriteFile(filepath.Join(ic.dir, imageCacheIndex), []byte(strings.Join(index, "\n")+"\n"), 0o644)
}

func (ic *imageCache) remove() {
	if ic.temporary {
		os.RemoveAll(ic.dir)
	}
}

// PreloadImages loads each image into the node from the image cache,
// pulling and caching it first when missing, then prunes the cache to what
// this run used.
func (c *Cluster) PreloadImages(ctx context.Context, images []string) error {
	arch, err := c.nodeArch(ctx)
	if err != nil {
		return err
	}
	for _, image := range images {
		archive := c.images.archive(archiveName(image, arch))
		if err := c.preloadImage(ctx, image, arch, archive); err != nil {
			return err
		}
	}
	return c.images.finish()
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
	if err := c.pullAndSave(ctx, image, image, arch, archive); err != nil {
		return err
	}
	if out, err := c.run(ctx, "kind", "load", "image-archive", archive, "--name", c.Name); err != nil {
		return fmt.Errorf("kind load image-archive %s: %w\n%s", image, err, out)
	}
	return nil
}

func (c *Cluster) pullAndSave(ctx context.Context, image, saveAs, arch, archive string) error {
	attempts, err := retryPull(ctx, pullBackoff, func() (string, error) {
		return c.run(ctx, c.Provider, "pull", "--platform", "linux/"+arch, image)
	}, func(attempt int, wait time.Duration, out string) {
		c.Log("image cache: pulling %s failed (attempt %d), retrying in %s:\n%s", image, attempt, wait, strings.TrimSpace(out))
	})
	if err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	c.Log("image cache: pulled %s from its registry (attempt %d)", image, attempts)
	if saveAs != image {
		if out, err := c.run(ctx, c.Provider, "tag", image, saveAs); err != nil {
			return fmt.Errorf("%s tag %s %s: %w\n%s", c.Provider, image, saveAs, err, out)
		}
	}
	return c.save(ctx, saveAs, arch, archive)
}

// save writes image to archive. docker save writes every platform of a
// multi-platform image unless told one, and fails on those it did not pull.
func (c *Cluster) save(ctx context.Context, image, arch, archive string) error {
	args := []string{"save", "-o", archive + ".partial"}
	if c.Provider == "docker" {
		args = append(args, "--platform", "linux/"+arch)
	}
	if out, err := c.run(ctx, c.Provider, append(args, image)...); err != nil {
		os.Remove(archive + ".partial")
		return fmt.Errorf("%s save %s: %w\n%s", c.Provider, image, err, out)
	}
	return os.Rename(archive+".partial", archive)
}

// ensureNodeImage makes the pinned kind node image available, from the
// cache when it can, and returns the name to create the cluster from. kind
// pulls unless `docker inspect` finds the image, and after save and load
// Docker's classic store no longer resolves a digest reference, so the image
// is handed to kind under a local tag carrying the digest.
func (c *Cluster) ensureNodeImage(ctx context.Context) (string, error) {
	pinned := c.Versions.Kind.NodeImage
	local := localNodeImage(pinned)
	archive := c.images.archive(archiveName(local, runtime.GOARCH))
	present := func() bool {
		return exec.CommandContext(ctx, c.Provider, "image", "inspect", local).Run() == nil
	}
	if present() {
		c.Log("image cache: %s is already in %s", local, c.Provider)
		return local, nil
	}
	if _, err := os.Stat(archive); err == nil {
		out, err := c.run(ctx, c.Provider, "load", "-i", archive)
		if err == nil && present() {
			c.Log("image cache: loaded %s from %s, with no registry pull", local, filepath.Base(archive))
			return local, nil
		}
		c.Log("image cache: loading %s failed, pulling it again: %v\n%s", filepath.Base(archive), err, out)
		os.Remove(archive)
	}
	return local, c.pullAndSave(ctx, pinned, local, runtime.GOARCH, archive)
}

// localNodeImage names the node image after the pinned digest (or tag), so
// a new pin gets a new name.
func localNodeImage(pinned string) string {
	id := pinned
	if _, digest, ok := strings.Cut(pinned, "@sha256:"); ok && len(digest) >= 16 {
		id = "sha256-" + digest[:16]
	} else if i := strings.LastIndex(pinned, ":"); i >= 0 && !strings.Contains(pinned[i:], "/") {
		id = pinned[i+1:]
	}
	return "iidp-e2e.local/kind-node:" + id
}

// pullBackoff totals about four minutes: enough for a per-minute limit to
// clear, well inside the job's timeout.
var pullBackoff = []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 80 * time.Second}

// retryPull retries pull while its failures look transient and returns the
// number of attempts made. Any other failure, such as a missing tag, is
// returned at once.
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

func transientPullError(out string) bool {
	return transientPull.MatchString(out)
}

// archiveName includes the platform, since the archive holds only the
// node's.
func archiveName(image, arch string) string {
	return strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(image) + "_linux-" + arch + ".tar"
}

func (c *Cluster) nodeArch(ctx context.Context) (string, error) {
	arch, err := c.Kubectl(ctx, "get", "nodes", "-o", "jsonpath={.items[0].status.nodeInfo.architecture}")
	arch = strings.TrimSpace(arch)
	if err != nil || arch == "" {
		return "", fmt.Errorf("node architecture: %v\n%s", err, arch)
	}
	return arch, nil
}

// LogImageSources logs, from the kubelet's events, which images the node
// pulled and which it found already present. Events of deleted namespaces
// are gone. Best-effort: it never fails the test.
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
