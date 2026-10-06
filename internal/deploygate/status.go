package deploygate

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
	"github.com/Itema-as/iidp/internal/platformstate"
	"github.com/Itema-as/iidp/internal/render"
	"github.com/Itema-as/iidp/internal/repoaccess"
)

// StatusPath is the read endpoint iidp app status calls, followed by the
// Application's name:
//
//	GET /v1/status/<app>
//	Authorization: Bearer <the developer's GitHub token, from gh auth>
//
// It answers 200 with a platformstate.Status, or an ErrorResponse. It
// reads, never writes, and does not use the App key.
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
	repo, err := repoaccess.Clone(ctx, g.PlatformRepo, dir, token, true, statusPurpose)
	if err != nil {
		return platformstate.Status{}, 0, accessRefusal(err)
	}

	environments, err := platformrepo.LiveEnvironments(dir, application)
	if err != nil {
		return platformstate.Status{}, 0, err
	}
	if len(environments) == 0 {
		return platformstate.Status{}, 0, refuse(http.StatusNotFound, "there is no Application %s on the Platform: %s has no Environment for it", application, platform.Repository)
	}
	readable, binding, err := repoaccess.Check(ctx, dir, application, g.OrgID, &github.Client{Token: token, BaseURL: g.GitHubAPI}, statusPurpose)
	if err != nil {
		return platformstate.Status{}, binding.RepositoryID, accessRefusal(err)
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
		if env.Image == nil || !slices.Contains(environments, env.Name) {
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
	for i := range envs {
		envs[i].DatabaseAccess = databaseAccess(dir, application, envs[i].Name)
	}
	if cfg, err := platformrepo.LoadConfig(dir); err == nil {
		for i := range envs {
			envs[i].Links = platformstate.LinksOf(cfg.ArgoCDURL, cfg.GrafanaURL, envs[i])
		}
	}
	return platformstate.Status{Application: application, Repository: readable.FullName, Environments: envs}, binding.RepositoryID, nil
}

// databaseAccess is an Environment's database access, from its values file
// in the clone at dir. A Preview Environment has staging's, since it
// renders from staging's values file. A level is set up when it is none or
// the file names its role's password Secret, as the chart needs to open
// the role. It is nil without Postgres, and when the file cannot be read
// or holds levels the chart would refuse.
func databaseAccess(dir, application, environment string) *platformstate.DatabaseAccess {
	if environment != "prod" {
		environment = "staging"
	}
	values, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(platformrepo.EnvironmentDir(application, environment)), "values.yaml"))
	if err != nil {
		return nil
	}
	db, err := render.ReadDatabase(values)
	if err != nil || !db.Enabled {
		return nil
	}
	setUp := func(role render.AccessRole) bool {
		return role.Level(db.Access) == render.AccessNone || db.PasswordSecret(role) != ""
	}
	return &platformstate.DatabaseAccess{
		ReadWrite:      db.Access.ReadWrite,
		ReadOnly:       db.Access.ReadOnly,
		ReadWriteSetUp: setUp(render.ReadWriteRole),
		ReadOnlySetUp:  setUp(render.ReadOnlyRole),
	}
}

// statusPurpose words the status endpoint's repository check.
var statusPurpose = repoaccess.Purpose{Service: "the Deploy gate", Action: "see %s status"}

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

// accessRefusal answers a repository check's refusal with its HTTP status.
func accessRefusal(err error) error {
	var ref *repoaccess.Refusal
	if errors.As(err, &ref) {
		return refuse(ref.Status(), "%s", ref.Message)
	}
	return err
}
