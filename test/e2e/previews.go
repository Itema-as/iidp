package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Preview Environments in kind (#95): ArgoCD's Pull Request generator asks
// the harness's fakegithub (test/e2e/testdata/fakegithub) for an
// Application repository's pull requests, as it would ask GitHub
// Enterprise, authenticating with the App credential the harness writes in
// place of cloud-init's. The test sets the pull requests the fake serves.

// FakePullRequest is a pull request the fake GitHub serves.
type FakePullRequest struct {
	Number  int
	Branch  string
	HeadSHA string
	Labels  []string
	// Closed pull requests are listed only for state=closed or all, never
	// to the generator, which asks for open ones.
	Closed bool
}

// SetPullRequests replaces the pull requests the fake GitHub serves for
// owner/repo, through a port-forward to it.
func (c *Cluster) SetPullRequests(ctx context.Context, owner, repo string, prs []FakePullRequest) error {
	type label struct {
		Name string `json:"name"`
	}
	type ref struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	type pull struct {
		Number int     `json:"number"`
		Title  string  `json:"title"`
		State  string  `json:"state"`
		Labels []label `json:"labels"`
		Head   ref     `json:"head"`
		Base   ref     `json:"base"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	list := make([]pull, 0, len(prs))
	for _, pr := range prs {
		p := pull{Number: pr.Number, Title: fmt.Sprintf("Pull request %d", pr.Number), State: "open", Labels: []label{},
			Head: ref{Ref: pr.Branch, SHA: pr.HeadSHA}, Base: ref{Ref: "main", SHA: strings.Repeat("0", 40)}}
		if pr.Closed {
			p.State = "closed"
		}
		p.User.Login = "e2e-developer"
		for _, l := range pr.Labels {
			p.Labels = append(p.Labels, label{Name: l})
		}
		list = append(list, p)
	}
	body, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return c.withPortForward(ctx, "fake-github", func(ctx context.Context, base string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/e2e/pulls/"+owner+"/"+repo, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("setting %s/%s's pull requests on the fake GitHub: %w", owner, repo, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("setting %s/%s's pull requests on the fake GitHub: %s", owner, repo, resp.Status)
		}
		c.Log("the fake GitHub now serves %d pull requests for %s/%s", len(prs), owner, repo)
		return nil
	})
}

// RefreshApplicationSet asks ArgoCD's ApplicationSet controller to run the
// named ApplicationSet's generators now rather than at its requeue time,
// the annotation ArgoCD documents for that.
func (c *Cluster) RefreshApplicationSet(ctx context.Context, name string) error {
	if out, err := c.Kubectl(ctx, "-n", "argocd", "annotate", "applicationset", name, "argocd.argoproj.io/application-set-refresh=true", "--overwrite"); err != nil {
		return fmt.Errorf("refresh ApplicationSet %s: %w\n%s", name, err, out)
	}
	return nil
}

// ApplicationSetConditions returns the named ApplicationSet's status
// conditions, type to "<status>: <message>".
func (c *Cluster) ApplicationSetConditions(ctx context.Context, name string) (map[string]string, error) {
	out, err := c.Kubectl(ctx, "-n", "argocd", "get", "applicationset", name, "-o", "jsonpath={.status.conditions}")
	if err != nil {
		return nil, fmt.Errorf("read ApplicationSet %s: %w\n%s", name, err, out)
	}
	var conditions []struct {
		Type    string `json:"type"`
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if strings.TrimSpace(out) != "" {
		if err := json.Unmarshal([]byte(out), &conditions); err != nil {
			return nil, fmt.Errorf("parse ApplicationSet %s conditions %q: %w", name, out, err)
		}
	}
	got := map[string]string{}
	for _, cond := range conditions {
		got[cond.Type] = cond.Status + ": " + cond.Message
	}
	return got, nil
}

// WaitForApplicationSetUpToDate polls until the named ApplicationSet has
// generated its Applications without an error: ResourcesUpToDate True and
// no ErrorOccurred True. On a Pull Request generator that proves the
// controller listed the repository's pull requests with the App.
func (c *Cluster) WaitForApplicationSetUpToDate(ctx context.Context, name string, timeout time.Duration) error {
	var last map[string]string
	return pollUntil(ctx, timeout, 5*time.Second,
		func() (bool, error) {
			conditions, err := c.ApplicationSetConditions(ctx, name)
			if err != nil {
				return false, nil
			}
			last = conditions
			return strings.HasPrefix(conditions["ResourcesUpToDate"], "True") && !strings.HasPrefix(conditions["ErrorOccurred"], "True"), nil
		},
		func() error {
			logs, _ := c.Kubectl(ctx, "-n", "argocd", "logs", "deployment/argocd-applicationset-controller", "--tail=40")
			fake, _ := c.Kubectl(ctx, "-n", GitServerNamespace, "logs", "deployment/fake-github", "--tail=20")
			return fmt.Errorf("ApplicationSet %s was not up to date within %s: %v\napplicationset-controller:\n%s\nfake-github:\n%s", name, timeout, last, logs, fake)
		})
}

// WaitForApplicationGone polls until ArgoCD no longer has the named
// Application: its resources finalizer has deleted everything it owned.
func (c *Cluster) WaitForApplicationGone(ctx context.Context, name string, timeout time.Duration) error {
	return pollUntil(ctx, timeout, 5*time.Second,
		func() (bool, error) {
			apps, err := c.Applications(ctx)
			if err != nil {
				return false, nil
			}
			_, ok := apps[name]
			return !ok, nil
		},
		func() error {
			out, _ := c.Kubectl(ctx, "-n", "argocd", "get", "application", name, "-o", "yaml")
			return fmt.Errorf("Application %s was not deleted within %s:\n%s", name, timeout, out)
		})
}

// TagNodeImage gives an image the kind node already has a second name,
// inside the node's containerd, so a Pod asking for target starts without
// a pull. Preview Environments run the image tagged with the pull
// request's head SHA, which no registry kind reaches has: this stands in
// for the deploy workflow pushing it. source is pulled first, in case the
// node does not have it yet.
func (c *Cluster) TagNodeImage(ctx context.Context, source, target string) error {
	node := c.Name + "-control-plane"
	for _, args := range [][]string{
		{"exec", node, "crictl", "pull", source},
		{"exec", node, "ctr", "-n", "k8s.io", "images", "tag", "--force", source, target},
	} {
		cmd := exec.CommandContext(ctx, c.Provider, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s %s: %w\n%s", c.Provider, strings.Join(args, " "), err, out)
		}
	}
	c.Log("tagged %s as %s in the node", source, target)
	return nil
}
