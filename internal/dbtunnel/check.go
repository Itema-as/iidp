package dbtunnel

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Itema-as/iidp/internal/dbtunnel/api"
	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
	"github.com/Itema-as/iidp/internal/platformstate"
	"github.com/Itema-as/iidp/internal/render"
	"github.com/Itema-as/iidp/internal/repoaccess"
)

// Cluster reads single objects from the Kubernetes API: the Environment's
// Cluster and its access roles' password Secrets. A missing object is a
// *platformstate.StatusError with Status 404.
type Cluster interface {
	Get(ctx context.Context, path string, into any) error
}

// Request is what one connection asks for.
type Request struct {
	Application string
	// Environment is prod, staging, pr-<number>, or api.EnvironmentAuto:
	// staging when the Application has one, else prod.
	Environment string
	ReadOnly    bool
	// Token is the developer's own GitHub token.
	Token string
}

// Grant is what the check lets a connection do: log in to an
// Environment's database as one role. It is filled as far as the check
// got, so a refusal is recorded with what was known.
type Grant struct {
	Login       string
	Application string
	Environment string
	// Namespace and Cluster are where the Environment's database is.
	Namespace string
	Cluster   string
	// Access is the role's level in words, read-write or read-only.
	Access string
	Role   string
	// Password is the role's password, which never leaves the tunnel.
	Password string
	// known is set once the Environment is known to exist, so a refusal
	// has an ArgoCD Application to be recorded on.
	known bool
}

// Refusal is a check that refused, with the HTTP status a check call
// answers it with.
type Refusal struct {
	Status  int
	Message string
}

func (r *Refusal) Error() string { return r.Message }

func refuse(status int, format string, args ...any) *Refusal {
	return &Refusal{Status: status, Message: fmt.Sprintf(format, args...)}
}

// purpose words the repository check for the tunnel.
var purpose = repoaccess.Purpose{Service: "the database tunnel", Action: "reach %s databases"}

// noToken refuses a call that carries no GitHub token.
const noToken = "no GitHub token: send your own as Authorization: Bearer <token>, which iidp app db connect takes from gh auth"

// check runs the full check for one connection, in order: the
// developer's GitHub login; the Environment, from the Platform repository
// cloned with their token, so that a refusal from here on is recorded on
// it; their permission on the Application repository, read the way the
// Deploy gate's status endpoint reads it; the Environment's access levels
// and roles, from its Cluster, which is what is deployed rather than what
// the Platform repository says; the role for the developer's permission;
// and that role's password.
func (t *Tunnel) check(ctx context.Context, req Request) (Grant, error) {
	grant := Grant{Application: req.Application, Environment: req.Environment}
	if err := platformrepo.ValidateName(req.Application); err != nil {
		return grant, refuse(http.StatusBadRequest, "%v", err)
	}
	switch env := req.Environment; {
	case env == "prod", env == "staging", env == api.EnvironmentAuto, platformstate.IsPreview(env):
	default:
		return grant, refuse(http.StatusBadRequest, "unknown Environment %q: must be prod, staging or pr-<pull request number>", env)
	}
	if req.Token == "" {
		return grant, refuse(http.StatusUnauthorized, noToken)
	}

	gh := &github.Client{Token: req.Token, BaseURL: t.GitHubAPI}
	login, err := gh.AuthenticatedLogin(ctx)
	switch {
	case github.IsUnauthorized(err):
		return grant, refuse(http.StatusUnauthorized, "GitHub does not accept your token; run gh auth login again")
	case err != nil:
		return grant, refuse(http.StatusBadGateway, "the database tunnel could not ask GitHub who you are: %v", err)
	}
	grant.Login = login

	dir, err := os.MkdirTemp("", "iidp-platform-tunnel-")
	if err != nil {
		return grant, err
	}
	defer os.RemoveAll(dir)
	if _, err := repoaccess.Clone(ctx, t.PlatformRepo, dir, req.Token, false, purpose); err != nil {
		return grant, accessRefusal(err)
	}
	environments, err := platformrepo.LiveEnvironments(dir, req.Application)
	if err != nil {
		return grant, err
	}
	if len(environments) == 0 {
		return grant, refuse(http.StatusNotFound, "there is no Application %s on the Platform: %s has no Environment for it", req.Application, platform.Repository)
	}
	if grant.Environment == api.EnvironmentAuto {
		grant.Environment = "prod"
		if slices.Contains(environments, "staging") {
			grant.Environment = "staging"
		}
	}
	env := render.Environment{Application: req.Application, Environment: grant.Environment}
	grant.Namespace = env.Name()
	grant.Cluster = env.Name() + "-db"
	if grant.Environment == "prod" {
		grant.Cluster = req.Application + "-db"
	}
	// A Preview Environment is only known once its Cluster is found.
	preview := platformstate.IsPreview(grant.Environment)
	if !preview && !slices.Contains(environments, grant.Environment) {
		return grant, refuse(http.StatusNotFound, "unknown Environment: %s has no %s Environment", req.Application, grant.Environment)
	}
	grant.known = !preview

	repo, binding, err := repoaccess.Check(ctx, dir, req.Application, t.OrgID, gh, purpose)
	if err != nil {
		return grant, accessRefusal(err)
	}

	var cluster postgresCluster
	err = t.Cluster.Get(ctx, "/apis/postgresql.cnpg.io/v1/namespaces/"+url.PathEscape(grant.Namespace)+"/clusters/"+url.PathEscape(grant.Cluster), &cluster)
	switch {
	case isNotFound(err) && preview:
		return grant, refuse(http.StatusNotFound, "there is no Preview Environment for pull request %s of %s with a database: label the pull request %s and give it a few minutes, or check its number", strings.TrimPrefix(grant.Environment, "pr-"), req.Application, render.PreviewLabel)
	case isNotFound(err):
		if hasDatabase(dir, req.Application, grant.Environment) {
			return grant, refuse(http.StatusConflict, "%s %s's database is not running yet: ArgoCD has not created it", req.Application, grant.Environment)
		}
		return grant, refuse(http.StatusConflict, "%s %s has no database; add one with iidp app add-capability %s --postgres", req.Application, grant.Environment, req.Application)
	case err != nil:
		return grant, refuse(http.StatusServiceUnavailable, "the database tunnel cannot read the cluster right now: %v", err)
	}
	grant.known = true

	access, ok := cluster.access()
	if !ok {
		return grant, refuse(http.StatusConflict, "refused: %s %s's database says nothing about who may reach it: its application chart is older than database access", req.Application, grant.Environment)
	}
	role, ok := ChooseRole(repo.Permission(), access, req.ReadOnly)
	if !ok {
		return grant, refuse(http.StatusForbidden, "%s", levelRefusal(req.Application, grant.Environment, binding.Repository, repo.Permission(), access, req.ReadOnly))
	}
	grant.Access, grant.Role = role.String(), role.PostgresRole(req.Application)

	secret, ok := cluster.passwordSecret(grant.Role)
	if !ok {
		return grant, refuse(http.StatusConflict, "%s", notSetUpRefusal(req.Application, grant.Environment))
	}
	password, err := t.password(ctx, grant.Namespace, secret, grant.Role)
	if err != nil {
		t.log().Warn("a database access role's password could not be read", "application", grant.Application, "environment", grant.Environment,
			"role", grant.Role, "secret", secret, "error", err.Error())
		return grant, refuse(http.StatusConflict, "%s", notSetUpRefusal(req.Application, grant.Environment))
	}
	grant.Password = password
	return grant, nil
}

// postgresCluster is the part of a postgresql.cnpg.io/v1 Cluster the
// check reads.
type postgresCluster struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Managed struct {
			Roles []struct {
				Name           string `json:"name"`
				Ensure         string `json:"ensure"`
				PasswordSecret struct {
					Name string `json:"name"`
				} `json:"passwordSecret"`
			} `json:"roles"`
		} `json:"managed"`
	} `json:"spec"`
}

// access is the levels the chart set on the Cluster, false when it set
// none.
func (c postgresCluster) access() (render.DatabaseAccess, bool) {
	readWrite, rw := c.Metadata.Annotations[platformstate.DatabaseAccessReadWriteAnnotation]
	readOnly, ro := c.Metadata.Annotations[platformstate.DatabaseAccessReadOnlyAnnotation]
	return render.DatabaseAccess{ReadWrite: readWrite, ReadOnly: readOnly}, rw && ro
}

// passwordSecret is the Secret holding role's password, as the Cluster
// names it, false when the role is absent or names none. A Preview
// Environment's roles name staging's Secrets.
func (c postgresCluster) passwordSecret(role string) (string, bool) {
	for _, r := range c.Spec.Managed.Roles {
		// CloudNativePG's default for ensure is present.
		if r.Name == role && (r.Ensure == "" || r.Ensure == "present") && r.PasswordSecret.Name != "" {
			return r.PasswordSecret.Name, true
		}
	}
	return "", false
}

// password reads role's password from the kubernetes.io/basic-auth Secret
// name in namespace, which must be for that role.
func (t *Tunnel) password(ctx context.Context, namespace, name, role string) (string, error) {
	var secret struct {
		Data map[string]string `json:"data"`
	}
	if err := t.Cluster.Get(ctx, "/api/v1/namespaces/"+url.PathEscape(namespace)+"/secrets/"+url.PathEscape(name), &secret); err != nil {
		return "", err
	}
	username, err := base64.StdEncoding.DecodeString(secret.Data["username"])
	if err != nil || string(username) != role {
		return "", fmt.Errorf("the Secret %s is not %s's: its username is %q", name, role, username)
	}
	password, err := base64.StdEncoding.DecodeString(secret.Data["password"])
	if err != nil || len(password) == 0 {
		return "", fmt.Errorf("the Secret %s holds no password", name)
	}
	return string(password), nil
}

// hasDatabase reports whether the Environment's values file in the clone
// at dir turns Postgres on.
func hasDatabase(dir, application, environment string) bool {
	values, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(platformrepo.EnvironmentDir(application, environment)), "values.yaml"))
	if err != nil {
		return false
	}
	db, err := render.ReadDatabase(values)
	return err == nil && db.Enabled
}

func isNotFound(err error) bool {
	var se *platformstate.StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

// accessRefusal is a repository check's refusal with its HTTP status.
func accessRefusal(err error) error {
	var ref *repoaccess.Refusal
	if errors.As(err, &ref) {
		return refuse(ref.Status(), "%s", ref.Message)
	}
	return err
}
