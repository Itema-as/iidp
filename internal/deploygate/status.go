package deploygate

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
	"github.com/Itema-as/iidp/internal/platformstate"
)

// StatusPath is the read endpoint iidp app status calls, followed by the
// Application's name:
//
//	GET /v1/status/<app>
//	Authorization: Bearer <the developer's GitHub token, from gh auth>
//
// It answers 200 with a platformstate.Status, or an ErrorResponse. It is
// the gate's service answering developers, not the Deploy gate itself: it
// reads, never writes, and does not use the App key
// (docs/adr/0007-app-status-reads-through-the-deploy-gate.md,
// docs/implementation-notes/94-app-status.md).
const StatusPath = "/v1/status/"

func (g *Gate) serveStatus(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	application := r.PathValue("application")
	res, repositoryID, err := g.status(r.Context(), r, application)
	attrs := []any{"application", application, "repository_id", repositoryID,
		"duration", time.Since(start).Round(time.Millisecond).String()}
	if err != nil {
		status := http.StatusInternalServerError
		var ref *refusal
		if errors.As(err, &ref) {
			status = ref.status
		}
		g.log().Warn("status refused", append(attrs, "status", status, "error", err.Error())...)
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
		return
	}
	g.log().Info("status", append(attrs, "environments", len(res.Environments))...)
	writeJSON(w, http.StatusOK, res)
}

// status checks, in order: a GitHub token; a valid name; the Platform
// repository, cloned with that token; the Application in it; its binding;
// and, with that token, that GitHub lets the user read the bound
// repository. Only then does it read the cluster.
func (g *Gate) status(ctx context.Context, r *http.Request, application string) (platformstate.Status, int64, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	token = strings.TrimSpace(token)
	if !ok || token == "" {
		return platformstate.Status{}, 0, refuse(http.StatusUnauthorized, "no GitHub token: send your own as Authorization: Bearer <token>, which iidp app status takes from gh auth")
	}
	if err := platformrepo.ValidateName(application); err != nil {
		return platformstate.Status{}, 0, refuse(http.StatusBadRequest, "%v", err)
	}

	dir, err := os.MkdirTemp("", "iidp-platform-status-")
	if err != nil {
		return platformstate.Status{}, 0, err
	}
	defer os.RemoveAll(dir)
	repo, err := git.CloneWithHistory(ctx, g.PlatformRepo, platformrepo.Branch, dir, git.Auth{Token: token})
	if err != nil {
		return platformstate.Status{}, 0, cloneRefusal(err)
	}

	environments, err := repositoryEnvironments(dir, application)
	if err != nil {
		return platformstate.Status{}, 0, err
	}
	if len(environments) == 0 {
		return platformstate.Status{}, 0, refuse(http.StatusNotFound, "there is no Application %s on the Platform: %s has no Environment for it", application, platform.Repository)
	}
	binding, ok, err := platformrepo.ReadRepositoryBinding(dir, application)
	if err != nil || !ok || !binding.Complete() {
		return platformstate.Status{}, 0, refuse(http.StatusForbidden, "refused: %s is not bound to an Application repository, so there is no repository whose readers may see its status. Bind it first: iidp app bind %s --repo %s/<repository>", application, application, platform.Org)
	}
	if binding.RepositoryOwnerID != g.OrgID {
		return platformstate.Status{}, binding.RepositoryID, refuse(http.StatusForbidden, "refused: %s is bound to a repository outside %s (owner id %d); rebind it with iidp app bind %s --repo %s/<repository> --rebind", application, platform.Org, binding.RepositoryOwnerID, application, platform.Org)
	}

	gh := &github.Client{Token: token, BaseURL: g.GitHubAPI}
	readable, err := gh.RepositoryByID(ctx, binding.RepositoryID)
	switch {
	case github.IsUnauthorized(err):
		return platformstate.Status{}, binding.RepositoryID, refuse(http.StatusUnauthorized, "GitHub does not accept your token; run gh auth login again")
	case github.IsNotFound(err), err == nil && !readable.CanPull:
		// GitHub answers 404, not 403, for a private repository the user
		// cannot see: both mean no access.
		return platformstate.Status{}, binding.RepositoryID, refuse(http.StatusForbidden, "refused: you cannot read %s's Application repository (%s, repository id %d) on GitHub, and only its readers may see %s's status. Ask for read access to it", application, binding.Repository, binding.RepositoryID, application)
	case err != nil:
		return platformstate.Status{}, binding.RepositoryID, refuse(http.StatusBadGateway, "the Deploy gate could not ask GitHub whether you can read %s's repository: %v", application, err)
	}

	if g.Cluster == nil {
		return platformstate.Status{}, binding.RepositoryID, refuse(http.StatusServiceUnavailable, "the Deploy gate cannot read the cluster: it has no Kubernetes access configured")
	}
	envs, err := platformstate.Read(ctx, g.Cluster, application)
	if err != nil {
		return platformstate.Status{}, binding.RepositoryID, refuse(http.StatusServiceUnavailable, "the Deploy gate cannot read the cluster right now: %v", err)
	}
	envs = withRepositoryEnvironments(envs, environments)
	for i := range envs {
		env := &envs[i]
		if env.Image == nil || !contains(environments, env.Name) {
			continue
		}
		at, ok, err := platformrepo.TagDeployedAt(ctx, repo, application, env.Name, env.Image.Tag)
		if err != nil {
			return platformstate.Status{}, binding.RepositoryID, err
		}
		if ok {
			env.Image.DeployedAt = &at
		}
	}
	if cfg, err := platformrepo.LoadConfig(dir); err == nil {
		for i := range envs {
			envs[i].Links = platformstate.LinksOf(cfg.ArgoCDURL, cfg.GrafanaURL, envs[i])
		}
	}
	return platformstate.Status{Application: application, Repository: readable.FullName, Environments: envs}, binding.RepositoryID, nil
}

// cloneRefusal words a failed clone of the Platform repository with the
// caller's token. GitHub answers "Repository not found" to a token that may
// not read it, and "Authentication failed" to one it does not accept.
func cloneRefusal(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "Authentication failed"), strings.Contains(msg, "Invalid username or token"):
		return refuse(http.StatusUnauthorized, "GitHub does not accept your token; run gh auth login again")
	case strings.Contains(msg, "not found"), strings.Contains(msg, "403"):
		return refuse(http.StatusForbidden, "refused: your GitHub login cannot read the Platform repository %s", platform.Repository)
	default:
		return refuse(http.StatusBadGateway, "the Deploy gate could not read the Platform repository %s: %v", platform.Repository, err)
	}
}

// repositoryEnvironments are the Environments the Platform repository has
// for application: those with a live application.yaml.
func repositoryEnvironments(dir, application string) ([]string, error) {
	var out []string
	for _, environment := range platformrepo.Environments {
		live, err := platformrepo.HasEnvironment(dir, application, environment)
		if err != nil {
			return nil, err
		}
		if live {
			out = append(out, environment)
		}
	}
	return out, nil
}

// withRepositoryEnvironments adds an entry, with no ArgoCD state, for each
// Environment the Platform repository has and the cluster does not yet:
// one just created, before ArgoCD picks it up.
func withRepositoryEnvironments(envs []platformstate.Environment, names []string) []platformstate.Environment {
	for _, name := range names {
		found := false
		for _, env := range envs {
			found = found || env.Name == name
		}
		if !found {
			envs = append(envs, platformstate.Environment{Name: name, Tasks: []platformstate.Task{}, Addresses: []string{}})
		}
	}
	platformstate.SortEnvironments(envs)
	return envs
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
