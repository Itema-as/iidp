// Package deploygate is the Deploy gate: the one service through which an
// Application repository's CI deploys and promotes. It holds the
// iidp-deploy GitHub App's key, which never reaches CI, and makes exactly
// one kind of change to the Platform repository: a new image tag for one
// Environment of the Application whose own repository asked, from a ref
// allowed to deploy that Environment, of a tag the image's registry has
// (docs/adr/0005-private-application-repositories-on-github-free.md,
// docs/implementation-notes/60-deploy-gate.md,
// docs/implementation-notes/61-image-check.md).
//
// The API is one call, POST /v1/deploy, authenticated with a GitHub
// Actions OIDC token for the gate's own URL:
//
//	POST /v1/deploy
//	Authorization: Bearer <OIDC token>
//	{"application": "shop", "environment": "auto", "tag": "<sha or version>"}
//
// It answers 200 with what it wrote, or an error status with
// {"error": "<what was refused and why>"}, which iidp ci set-image prints
// for the developer.
package deploygate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/githubapp"
	"github.com/Itema-as/iidp/internal/oidc"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
	"github.com/Itema-as/iidp/internal/registry"
)

// DeployPath is the gate's one call.
const DeployPath = "/v1/deploy"

// MainRef is the ref that deploys, and TagRefPrefix the refs that promote.
const (
	MainRef      = "refs/heads/main"
	TagRefPrefix = "refs/tags/v"
)

// Request is the body of a deploy call.
type Request struct {
	Application string `json:"application"`
	// Environment is prod, staging or auto. auto lets the gate decide from
	// the ref and the Platform repository.
	Environment string `json:"environment"`
	Tag         string `json:"tag"`
}

// Response is what a successful call wrote.
type Response struct {
	Application string `json:"application"`
	Environment string `json:"environment"`
	Tag         string `json:"tag"`
	// File is the values file, relative to the Platform repository.
	File string `json:"file"`
	// Commit is the Platform repository commit, "" when Unchanged.
	Commit string `json:"commit,omitempty"`
	// Unchanged is true when the Environment already ran Tag.
	Unchanged bool `json:"unchanged,omitempty"`
}

// ErrorResponse is the body of every refusal.
type ErrorResponse struct {
	Error string `json:"error"`
}

// AppCredentials are the iidp-deploy GitHub App's id, its installation on
// the org, and its private key.
type AppCredentials struct {
	AppID          int64
	InstallationID int64
	PrivateKey     []byte
}

// CredentialsFromDir reads AppCredentials from the files a Kubernetes
// Secret volume of ArgoCD's Platform-repository credential holds
// (githubAppID, githubAppInstallationID, githubAppPrivateKey; see
// docs/implementation-notes/41-argocd-platform-repo-credential.md). They
// are read on every call, so a rotated key is picked up once the kubelet
// refreshes the volume, with no restart.
func CredentialsFromDir(dir string) func() (AppCredentials, error) {
	return func() (AppCredentials, error) {
		read := func(name string) (string, error) {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return "", fmt.Errorf("reading the GitHub App credential: %w", err)
			}
			return strings.TrimSpace(string(data)), nil
		}
		var creds AppCredentials
		for name, into := range map[string]*int64{"githubAppID": &creds.AppID, "githubAppInstallationID": &creds.InstallationID} {
			v, err := read(name)
			if err != nil {
				return AppCredentials{}, err
			}
			if *into, err = strconv.ParseInt(v, 10, 64); err != nil {
				return AppCredentials{}, fmt.Errorf("the GitHub App credential's %s %q is not a number", name, v)
			}
		}
		key, err := read("githubAppPrivateKey")
		if err != nil {
			return AppCredentials{}, err
		}
		creds.PrivateKey = []byte(key + "\n")
		return creds, nil
	}
}

// Gate is the Deploy gate's HTTP service.
type Gate struct {
	// OIDC verifies the caller's token: issuer, audience (the gate's own
	// URL), signature and lifetime.
	OIDC *oidc.Verifier
	// OrgID is the numeric id of the org whose repositories may deploy
	// (Itema-as). Both the token's repository_owner_id and the
	// Application's binding must carry it.
	OrgID int64
	// PlatformRepo is the git URL of the Platform repository.
	PlatformRepo string
	// GitHubAPI is the GitHub REST API root; "" means the real one.
	GitHubAPI string
	// Credentials yields the App's credentials for each call.
	Credentials func() (AppCredentials, error)
	// Images checks that a tag exists in the Environment's image
	// repository before it is committed. Nil refuses every deploy: the
	// gate never commits a tag unchecked.
	Images *registry.Checker
	// Log receives one line per call; nil discards.
	Log *slog.Logger
	// BeforePush, when set, runs between the commit and each push. Tests
	// use it to move main.
	BeforePush func() error

	// writeMu serialises writes to the Platform repository: concurrent
	// deploys queue here instead of racing each other's pushes.
	writeMu sync.Mutex
	// botMu guards bot, the App's commit identity, looked up once.
	botMu sync.Mutex
	bot   git.Identity
}

// Handler serves the gate's routes.
func (g *Gate) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("POST "+DeployPath, g.serveDeploy)
	return mux
}

// refusal is an error the caller caused, with the status to answer it with.
type refusal struct {
	status int
	msg    string
}

func (r *refusal) Error() string { return r.msg }

func refuse(status int, format string, args ...any) error {
	return &refusal{status: status, msg: fmt.Sprintf(format, args...)}
}

func (g *Gate) log() *slog.Logger {
	if g.Log == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return g.Log
}

func (g *Gate) serveDeploy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	res, claims, req, err := g.deploy(r)
	attrs := []any{
		"repository", claims.Repository, "repository_id", claims.RepositoryID.String(),
		"ref", claims.Ref, "actor", claims.Actor,
		"application", req.Application, "environment", req.Environment, "tag", req.Tag,
		"duration", time.Since(start).Round(time.Millisecond).String(),
	}
	if err != nil {
		status := http.StatusInternalServerError
		var ref *refusal
		switch {
		case errors.As(err, &ref):
			status = ref.status
		case errors.Is(err, platformrepo.ErrApplicationMissing), errors.Is(err, platformrepo.ErrEnvironmentMissing):
			status = http.StatusNotFound
		}
		g.log().Warn("deploy refused", append(attrs, "status", status, "error", err.Error())...)
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
		return
	}
	g.log().Info("deployed", append(attrs, "resolved", res.Environment, "commit", res.Commit, "unchanged", res.Unchanged)...)
	writeJSON(w, http.StatusOK, res)
}

// tagPattern is the grammar of an OCI image tag.
var tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func (g *Gate) deploy(r *http.Request) (Response, oidc.Claims, Request, error) {
	var req Request
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return Response{}, oidc.Claims{}, req, refuse(http.StatusUnauthorized, "no GitHub Actions OIDC token: send it as Authorization: Bearer <token>; the workflow needs permissions: id-token: write")
	}
	claims, err := g.OIDC.Verify(r.Context(), strings.TrimSpace(token))
	if err != nil {
		if errors.Is(err, oidc.ErrInvalidToken) {
			return Response{}, claims, req, refuse(http.StatusUnauthorized, "refused: %v", err)
		}
		return Response{}, claims, req, refuse(http.StatusServiceUnavailable, "the Deploy gate cannot verify the token right now: %v", err)
	}

	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return Response{}, claims, req, refuse(http.StatusBadRequest, "the request body is not a deploy request: %v", err)
	}
	if err := platformrepo.ValidateName(req.Application); err != nil {
		return Response{}, claims, req, refuse(http.StatusBadRequest, "%v", err)
	}
	switch req.Environment {
	case "prod", "staging", platformrepo.EnvironmentAuto:
	default:
		return Response{}, claims, req, refuse(http.StatusBadRequest, "unknown Environment %q: must be prod, staging or auto", req.Environment)
	}
	if !tagPattern.MatchString(req.Tag) {
		return Response{}, claims, req, refuse(http.StatusBadRequest, "image tag %q is not a valid tag: letters, digits, _, . and -, at most 128 characters, not starting with . or -", req.Tag)
	}

	if !claims.RepositoryOwnerID.Is(g.OrgID) {
		return Response{}, claims, req, refuse(http.StatusForbidden, "refused: %s belongs to %s (owner id %s), and only repositories in %s (owner id %d) can deploy to the Platform", claims.Repository, claims.RepositoryOwner, claims.RepositoryOwnerID, platform.Org, g.OrgID)
	}
	promote := strings.HasPrefix(claims.Ref, TagRefPrefix)
	if claims.Ref != MainRef && !promote {
		return Response{}, claims, req, refuse(http.StatusForbidden, "refused: %s may not deploy %s. Only %s deploys (to staging, or to prod when there is no staging) and a v* tag (%s*) promotes to prod", claims.Ref, req.Application, MainRef, TagRefPrefix)
	}
	if claims.Actor == "" || claims.ActorID == "" {
		return Response{}, claims, req, refuse(http.StatusUnauthorized, "refused: the token names no actor to record the deploy as")
	}

	creds, err := g.Credentials()
	if err != nil {
		return Response{}, claims, req, err
	}

	g.writeMu.Lock()
	defer g.writeMu.Unlock()

	appClient, token, err := g.installationToken(r.Context(), creds)
	if err != nil {
		return Response{}, claims, req, err
	}
	bot, err := g.botIdentity(r.Context(), appClient, &github.Client{Token: token, BaseURL: g.GitHubAPI})
	if err != nil {
		return Response{}, claims, req, err
	}

	writer := &platformrepo.Writer{
		URL: g.PlatformRepo,
		Auth: git.Auth{
			Token:     token,
			Identity:  git.Identity{Name: claims.Actor, Email: claims.ActorID.String() + "+" + claims.Actor + "@users.noreply.github.com"},
			Committer: bot,
		},
		BeforePush: g.BeforePush,
	}
	body := fmt.Sprintf("Through the Deploy gate, for %s (repository id %s) at %s,\n%s, workflow %s.",
		claims.Repository, claims.RepositoryID, claims.SHA, claims.Ref, claims.WorkflowRef)
	res, err := writer.SetImageTag(r.Context(), platformrepo.ImageTagChange{
		Application: req.Application,
		Tag:         req.Tag,
		Body:        body,
		Environment: func(dir string) (string, error) {
			environment, err := g.authorize(dir, claims, req, promote)
			if err != nil {
				return "", err
			}
			return environment, g.checkImage(r.Context(), dir, req.Application, environment, req.Tag)
		},
	})
	if err != nil {
		return Response{}, claims, req, err
	}
	return Response{
		Application: req.Application,
		Environment: res.Environment,
		Tag:         req.Tag,
		File:        res.File,
		Commit:      res.Commit,
		Unchanged:   res.Unchanged,
	}, claims, req, nil
}

// authorize runs on the clone the write is made from: the Application
// must be bound to the calling repository, and the ref must be allowed to
// deploy the Environment asked for. It returns the Environment to write.
func (g *Gate) authorize(dir string, claims oidc.Claims, req Request, promote bool) (string, error) {
	binding, ok, err := platformrepo.ReadRepositoryBinding(dir, req.Application)
	if err != nil || !ok || !binding.Complete() {
		return "", refuse(http.StatusForbidden, "refused: %s is not bound to an Application repository, so nothing may deploy it. Bind it to its repository first: iidp app bind %s --repo %s", req.Application, req.Application, claims.Repository)
	}
	if binding.RepositoryOwnerID != g.OrgID {
		return "", refuse(http.StatusForbidden, "refused: %s is bound to a repository outside %s (owner id %d); rebind it with iidp app bind %s --repo %s/<repository> --rebind", req.Application, platform.Org, binding.RepositoryOwnerID, req.Application, platform.Org)
	}
	if !claims.RepositoryID.Is(binding.RepositoryID) {
		return "", refuse(http.StatusForbidden, "refused: %s is bound to %s (repository id %d), and this call comes from %s (repository id %s). Only an Application's own repository can deploy it", req.Application, binding.Repository, binding.RepositoryID, claims.Repository, claims.RepositoryID)
	}

	hasStaging, err := platformrepo.HasEnvironment(dir, req.Application, "staging")
	if err != nil {
		return "", err
	}
	target := "prod"
	if !promote && hasStaging {
		target = "staging"
	}
	if req.Environment == platformrepo.EnvironmentAuto || req.Environment == target {
		return target, nil
	}
	exists, err := platformrepo.HasEnvironment(dir, req.Application, req.Environment)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("%w: %s has no %s Environment", platformrepo.ErrEnvironmentMissing, req.Application, req.Environment)
	}
	if promote {
		return "", refuse(http.StatusForbidden, "refused: %s promotes to prod only, not %s", claims.Ref, req.Environment)
	}
	return "", refuse(http.StatusForbidden, "refused: %s deploys %s to staging, because it has one; prod is promoted by pushing a v* tag", MainRef, req.Application)
}

// installationToken mints an installation token for the App. It returns
// the GitHub client authenticated as the App itself (a JWT) and the token
// the Platform repository is written with. The token is minted per call:
// it costs one request, and the key is read fresh each time.
func (g *Gate) installationToken(ctx context.Context, creds AppCredentials) (*github.Client, string, error) {
	jwt, err := githubapp.SignJWT(creds.AppID, creds.PrivateKey, time.Now())
	if err != nil {
		return nil, "", err
	}
	client := &github.Client{Token: jwt, BaseURL: g.GitHubAPI}
	token, err := client.CreateInstallationToken(ctx, creds.InstallationID)
	if err != nil {
		return nil, "", err
	}
	return client, token, nil
}

// botIdentity is the App's bot account as a git identity, looked up once
// (GET /app for the slug, GET /users/<slug>[bot] for its id) and cached:
// <slug>[bot] <<id>+<slug>[bot]@users.noreply.github.com>, the address
// GitHub links to the bot's commits. GET /app takes the App's JWT; GET
// /users does not, so it goes through installation, the client
// authenticated with the installation token.
func (g *Gate) botIdentity(ctx context.Context, appClient, installation *github.Client) (git.Identity, error) {
	g.botMu.Lock()
	defer g.botMu.Unlock()
	if g.bot.Name != "" {
		return g.bot, nil
	}
	slug, err := appClient.AppSlug(ctx)
	if err != nil {
		return git.Identity{}, err
	}
	login := slug + "[bot]"
	id, err := installation.UserID(ctx, login)
	if err != nil {
		return git.Identity{}, err
	}
	g.bot = git.Identity{Name: login, Email: strconv.FormatInt(id, 10) + "+" + login + "@users.noreply.github.com"}
	return g.bot, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
