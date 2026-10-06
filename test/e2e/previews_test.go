//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/platformstate"
)

// previewHeadSHA is the head SHA, and so the image tag, of the pull request
// that gets a preview.
const previewHeadSHA = "5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f"

// testPreviewEnvironments proves notes's previews ApplicationSet:
//   - lists the pull requests with the GitHub App's credential and makes a
//     preview only for the open one labelled preview;
//   - runs its image in a namespace the guardrails bind to, with a database
//     and staging's migration and secrets but no backups and no Scheduled
//     task, behind Itema login;
//   - lets a developer reach its database through the database tunnel with
//     staging's levels;
//   - deletes the preview, its database and namespace when the pull request
//     closes.
func testPreviewEnvironments(ctx context.Context, t *testing.T, cluster *Cluster) {
	t.Helper()
	const (
		set       = "notes-previews"
		preview   = "notes-pr-7"
		namespace = "notes-pr-7"
	)
	if err := cluster.WaitForApplicationSetUpToDate(ctx, set, 3*time.Minute); err != nil {
		t.Fatal(err)
	}

	image := "docker.io/nginxinc/nginx-unprivileged:" + previewHeadSHA
	if err := cluster.TagNodeImage(ctx, "docker.io/nginxinc/nginx-unprivileged:1.30-alpine", image); err != nil {
		t.Fatal(err)
	}
	open := []FakePullRequest{
		{Number: 7, Branch: "feature/search", HeadSHA: previewHeadSHA, Labels: []string{"enhancement", "preview"}},
		{Number: 8, Branch: "fix/typo", HeadSHA: strings.Repeat("8", 40), Labels: []string{"bug"}},
	}
	if err := cluster.SetPullRequests(ctx, "Itema-as", "notes", open); err != nil {
		t.Fatal(err)
	}
	if err := cluster.RefreshApplicationSet(ctx, set); err != nil {
		t.Fatal(err)
	}
	if err := cluster.WaitForApplications(ctx, map[string]Expectation{preview: Healthy}, 8*time.Minute); err != nil {
		t.Fatal(err)
	}
	apps, err := cluster.Applications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := apps["notes-pr-8"]; ok {
		t.Error("pull request 8, without the preview label, got a preview")
	}
	// iidp app status finds Environments by these labels.
	out, err := cluster.Kubectl(ctx, "-n", "argocd", "get", "applications.argoproj.io", "-l", "iidp.itema.no/application=notes,iidp.itema.no/environment=pr-7", "-o", "name")
	if err != nil || strings.TrimSpace(out) != "application.argoproj.io/"+preview {
		t.Errorf("Applications labelled notes, pr-7: %q (err %v), want %s", out, err, preview)
	}

	testStatusListsPreview(ctx, t, cluster, "notes", "pr-7")
	testPreviewThroughTheTunnel(ctx, t, cluster, "notes", "7")

	out, err = cluster.Kubectl(ctx, "get", "namespace", namespace, "-o", "jsonpath={.metadata.labels}")
	if err != nil {
		t.Fatalf("read namespace %s: %v\n%s", namespace, err, out)
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(out), &labels); err != nil {
		t.Fatalf("namespace %s labels %q: %v", namespace, out, err)
	}
	for key, want := range map[string]string{
		"iidp.itema.no/application":          "notes",
		"iidp.itema.no/environment":          "pr-7",
		"pod-security.kubernetes.io/enforce": "restricted",
		"pod-security.kubernetes.io/warn":    "restricted",
		"pod-security.kubernetes.io/audit":   "restricted",
	} {
		if labels[key] != want {
			t.Errorf("namespace %s label %s = %q, want %q", namespace, key, labels[key], want)
		}
	}

	if got, _ := cluster.Kubectl(ctx, "-n", namespace, "get", "deployment", preview, "-o", "jsonpath={.spec.template.spec.containers[0].image}"); strings.TrimSpace(got) != image {
		t.Errorf("the preview runs %q, want %s", got, image)
	}
	if err := cluster.WaitForJobSucceeded(ctx, namespace, preview+"-migrate", time.Minute); err != nil {
		t.Error(err)
	}
	if got, _ := cluster.Kubectl(ctx, "-n", namespace, "get", "cluster.postgresql.cnpg.io", preview+"-db", "-o", "jsonpath={.spec.plugins}"); strings.TrimSpace(got) != "" {
		t.Errorf("the preview's database has plugins %s, want no WAL archiving", got)
	}
	out, _ = cluster.Kubectl(ctx, "-n", namespace, "get", "objectstores.barmancloud.cnpg.io,scheduledbackups.postgresql.cnpg.io,cronjobs", "-o", "name")
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); strings.Contains(line, "/") && !strings.Contains(line, " ") {
			t.Errorf("the preview has %s, want no backups and no Scheduled task", line)
		}
	}
	if exists, err := cluster.ResourceExists(ctx, "secret", namespace, "backups-credentials"); err != nil || !exists {
		t.Errorf("the preview has no backups-credentials Secret from staging's sops/ (err %v)", err)
	}

	if err := cluster.CheckSignInRedirect(ctx, preview+".app.example.test", signInPath, fixtureSignIn, 2*time.Minute); err != nil {
		t.Fatal(err)
	}

	events, err := cluster.AuditLog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, e := range events {
		if e.ObjectRef.Namespace != namespace {
			continue
		}
		seen++
		if failure, ok := e.Annotations[validationFailure]; ok {
			t.Errorf("%s %s/%s in %s failed a guardrail: %s", e.Verb, e.ObjectRef.Resource, e.ObjectRef.Name, namespace, failure)
		}
	}
	if seen == 0 {
		t.Errorf("the audit log has no writes in %s", namespace)
	}

	open[0].Closed = true
	if err := cluster.SetPullRequests(ctx, "Itema-as", "notes", open); err != nil {
		t.Fatal(err)
	}
	if err := cluster.RefreshApplicationSet(ctx, set); err != nil {
		t.Fatal(err)
	}
	if err := cluster.WaitForApplicationGone(ctx, preview, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := cluster.WaitForResourceGone(ctx, "namespace", namespace, namespace, 3*time.Minute); err != nil {
		t.Fatal(err)
	}
}

// testStatusListsPreview polls the gate's status endpoint for application
// until it lists environment.
func testStatusListsPreview(ctx context.Context, t *testing.T, cluster *Cluster, application, environment string) {
	t.Helper()
	var last string
	err := pollUntil(ctx, 2*time.Minute, 5*time.Second,
		func() (bool, error) {
			code, body, err := cluster.CallStatus(ctx, DeveloperToken, application)
			if err != nil {
				return false, err
			}
			last = fmt.Sprintf("HTTP %d: %s", code, body)
			if code >= 500 {
				return false, nil
			}
			if code != http.StatusOK {
				return false, fmt.Errorf("the status of %s: %s", application, last)
			}
			var status platformstate.Status
			if err := json.Unmarshal(body, &status); err != nil {
				return false, err
			}
			for _, env := range status.Environments {
				if env.Name == environment && env.ArgoCD != nil {
					return true, nil
				}
			}
			return false, nil
		},
		func() error {
			return fmt.Errorf("iidp app status %s never listed %s: %s", application, environment, last)
		})
	if err != nil {
		t.Error(err)
		return
	}
	t.Logf("iidp app status %s: %s", application, last)
}
