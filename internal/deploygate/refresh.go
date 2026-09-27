package deploygate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Itema-as/iidp/internal/platformstate"
	"github.com/Itema-as/iidp/internal/render"
)

// After a Deploy or Promote commit, the gate asks ArgoCD to refresh the
// Environment's ArgoCD Application straight away instead of waiting for
// its 3-minute poll of the Platform repository. It sets the annotation
// argocd.argoproj.io/refresh: normal, as ArgoCD's own webhook handler
// does; ArgoCD's controller then compares against the branch's newest
// commit and removes the annotation. This is the gate's one write in the
// cluster (docs/adr/0007-app-status-reads-through-the-deploy-gate.md,
// docs/implementation-notes/114-argocd-refresh.md).

// RefreshAnnotation is the annotation that asks ArgoCD to refresh an
// ArgoCD Application, and RefreshNormal the kind of refresh asked for: a
// fresh look at the Platform repository, without "hard"'s manifest cache
// invalidation, which a new commit does not need.
const (
	RefreshAnnotation = "argocd.argoproj.io/refresh"
	RefreshNormal     = "normal"
)

// refreshPatch is the JSON merge patch that sets the annotation and
// leaves every other one alone.
var refreshPatch = []byte(`{"metadata":{"annotations":{"` + RefreshAnnotation + `":"` + RefreshNormal + `"}}}`)

// refreshTimeout bounds the refresh request. The commit is already pushed
// by then, so a slow API server only delays the answer to CI.
const refreshTimeout = 10 * time.Second

// Patcher applies a JSON merge patch to the Kubernetes object at path,
// such as /apis/argoproj.io/v1alpha1/namespaces/argocd/applications/shop-prod.
type Patcher interface {
	MergePatch(ctx context.Context, path string, patch []byte) error
}

// ArgoCDApplicationPath is the API path of the ArgoCD Application named
// name.
func ArgoCDApplicationPath(name string) string {
	return "/apis/argoproj.io/v1alpha1/namespaces/" + platformstate.ArgoCDNamespace + "/applications/" + url.PathEscape(name)
}

// requestRefresh asks ArgoCD to refresh environment's ArgoCD Application,
// <application>-<environment>, the name the CLI gives it
// (render.Environment.Name). It runs only after a commit, and never fails
// the Deploy: a failure is logged, and ArgoCD's poll still picks the
// commit up. It does not follow the request's context, so a caller that
// hangs up once the commit is made still gets the refresh.
func (g *Gate) requestRefresh(ctx context.Context, application, environment, commit string) {
	if g.ArgoCD == nil {
		return
	}
	name := render.Environment{Application: application, Environment: environment}.Name()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	if err := g.ArgoCD.MergePatch(ctx, ArgoCDApplicationPath(name), refreshPatch); err != nil {
		g.log().Warn("argocd refresh failed; ArgoCD's poll will pick the commit up",
			"application", application, "environment", environment, "argocd_application", name,
			"commit", commit, "error", err.Error())
	}
}

// KubePatcher sends merge patches over the same connection, as the same
// service account, as the status reads (platformstate.Kube).
type KubePatcher struct {
	Kube *platformstate.Kube
}

// MergePatch implements Patcher.
func (p KubePatcher) MergePatch(ctx context.Context, path string, patch []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, p.Kube.BaseURL+path, bytes.NewReader(patch))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set("Accept", "application/json")
	if p.Kube.Token != nil {
		token, err := p.Kube.Token()
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := p.Kube.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("patching %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return &platformstate.StatusError{Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}
