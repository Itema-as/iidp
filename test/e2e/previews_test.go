//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// previewHeadSHA is the head SHA of the fixture pull request that gets a
// preview: the tag its image has, as the reusable deploy workflow pushes it.
const previewHeadSHA = "5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f"

// testPreviewEnvironments proves #95 on the cluster: notes's previews
// ApplicationSet (the fixture's copy of what the CLI writes), through
// ArgoCD's Pull Request generator and the harness's fake GitHub,
//   - lists the repository's pull requests with the App credential and
//     makes a preview only for the open one labelled preview;
//   - runs that pull request's image in namespace notes-pr-7, a namespace
//     the guardrails bind to, with a database and staging's migration and
//     secrets but no backups and no Scheduled task, behind Itema login;
//   - deletes the preview, its database and its namespace when the pull
//     request closes, with no backup to wait for.
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

	// The deploy workflow would have pushed the image; kind reaches no
	// registry that has it, so the node gets it under that name.
	image := "docker.io/library/nginx:" + previewHeadSHA
	if err := cluster.TagNodeImage(ctx, "docker.io/library/nginx:1.30-alpine", image); err != nil {
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
	// The preview's ArgoCD Application carries the labels every
	// Environment's does, which iidp app status finds Environments by.
	out, err := cluster.Kubectl(ctx, "-n", "argocd", "get", "applications.argoproj.io", "-l", "iidp.itema.no/application=notes,iidp.itema.no/environment=pr-7", "-o", "name")
	if err != nil || strings.TrimSpace(out) != "application.argoproj.io/"+preview {
		t.Errorf("Applications labelled notes, pr-7: %q (err %v), want %s", out, err, preview)
	}

	// The namespace carries the labels every Environment's does, so the
	// guardrails bind to it, and the tracking annotation that makes it
	// the Application's.
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
		"pod-security.kubernetes.io/enforce": "baseline",
		"pod-security.kubernetes.io/warn":    "restricted",
		"pod-security.kubernetes.io/audit":   "restricted",
	} {
		if labels[key] != want {
			t.Errorf("namespace %s label %s = %q, want %q", namespace, key, labels[key], want)
		}
	}

	// The pull request's image; a database without backups, migrated with
	// staging's command; staging's Secret through staging's sops/ source;
	// staging's Scheduled task not run.
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

	// Behind Itema login, through the preview's own Middleware for
	// staging's sign-in group.
	if err := cluster.CheckSignInRedirect(ctx, preview+".app.example.test", signInPath, fixtureSignIn, 2*time.Minute); err != nil {
		t.Fatal(err)
	}

	// Nothing the preview did failed a guardrail.
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

	// The pull request closes: the preview goes, its namespace and database
	// with it, and nothing waits on a backup.
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
