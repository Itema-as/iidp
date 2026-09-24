package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// DefaultBaseURL is the GitHub REST API tests point elsewhere.
const DefaultBaseURL = "https://api.github.com"

// Client is a small GitHub REST API client for what the CLI needs beyond
// git: reading the token's scopes, reading and creating Application
// repositories and opening Adopt's pull request. BaseURL and HTTPClient are
// injectable so tests run against an in-process fake server.
type Client struct {
	// BaseURL is the API root. Empty means DefaultBaseURL.
	BaseURL string
	// Token authenticates every request as the developer.
	Token string
	// HTTPClient makes the requests. Nil means http.DefaultClient.
	HTTPClient *http.Client
}

// NewClient makes a Client for the real GitHub API, authenticated with
// token.
func NewClient(token string) *Client {
	return &Client{Token: token}
}

// statusError is returned by do when GitHub answers with a non-2xx status.
type statusError struct {
	status int
	body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("GitHub API: HTTP %d: %s", e.status, e.body)
}

// IsNotFound reports whether err is the "404 Not Found" GitHub returns for
// a repository that does not exist.
func IsNotFound(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.status == http.StatusNotFound
}

// RepositoryExists reports whether owner/name already exists
// (GET /repos/{owner}/{name}).
func (c *Client) RepositoryExists(ctx context.Context, owner, name string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/repos/"+owner+"/"+name, nil, nil)
	switch {
	case err == nil:
		return true, nil
	case IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("checking whether %s/%s already exists: %w", owner, name, err)
	}
}

// Owner is a repository's owner as GitHub reports it: the login, which can
// change, and the numeric id, which never does.
type Owner struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

// CreatedRepository is what CreateRepository reads back from GitHub about
// the repository it made.
type CreatedRepository struct {
	// CloneURL is the git URL the first commit is pushed to.
	CloneURL string
	// DefaultBranch is the default branch GitHub gave the repository, so
	// the caller can tell whether SetDefaultBranch still needs to run.
	DefaultBranch string
	// ID is the repository's numeric id: what a GitHub Actions OIDC token
	// carries as repository_id, and what the Platform binds the
	// Application to (docs/platform-repository.md).
	ID int64
	// Owner is the org the repository was created in; its ID is the OIDC
	// token's repository_owner_id.
	Owner Owner
}

// CreateRepository creates a new repository named name in org
// (POST /orgs/{org}/repos) and returns what GitHub reports about it. It
// sets auto_init to false: Create always pushes its own first commit.
// Application repositories are only ever created in an org
// (docs/adr/0005-private-application-repositories-on-github-free.md).
func (c *Client) CreateRepository(ctx context.Context, org, name string, private bool) (CreatedRepository, error) {
	body := map[string]any{
		"name":      name,
		"private":   private,
		"auto_init": false,
	}
	var repo struct {
		CloneURL      string `json:"clone_url"`
		DefaultBranch string `json:"default_branch"`
		ID            int64  `json:"id"`
		Owner         Owner  `json:"owner"`
	}
	if err := c.do(ctx, http.MethodPost, "/orgs/"+org+"/repos", body, &repo); err != nil {
		return CreatedRepository{}, fmt.Errorf("creating %s/%s: %w", org, name, err)
	}
	if repo.CloneURL == "" {
		return CreatedRepository{}, fmt.Errorf("creating %s/%s: GitHub returned no clone_url", org, name)
	}
	return CreatedRepository{
		CloneURL:      repo.CloneURL,
		DefaultBranch: repo.DefaultBranch,
		ID:            repo.ID,
		Owner:         repo.Owner,
	}, nil
}

// SetDefaultBranch sets owner/name's default branch. Create calls it after
// pushing the initial commit, in case the repository was created with a
// different default (or none) than main.
func (c *Client) SetDefaultBranch(ctx context.Context, owner, name, branch string) error {
	body := map[string]any{"default_branch": branch}
	if err := c.do(ctx, http.MethodPatch, "/repos/"+owner+"/"+name, body, nil); err != nil {
		return fmt.Errorf("setting the default branch of %s/%s to %s: %w", owner, name, branch, err)
	}
	return nil
}

// Repository is what GetRepository reads about an existing repository: the
// fields the Adopt path needs (docs/implementation-notes/15-cli-adopt-path.md)
// and the ids the Platform binds an Application to
// (docs/implementation-notes/58-repository-binding.md). CanPush is
// GitHub's own view of the authenticated token's permission, present in
// the response only because the request is authenticated.
type Repository struct {
	// FullName is owner/name as GitHub reports it now. It differs from
	// what was asked for when the repository was renamed or transferred,
	// since GitHub redirects the old name to the new one.
	FullName      string
	DefaultBranch string
	CloneURL      string
	CanPush       bool
	// ID is the repository's numeric id, the OIDC token's repository_id.
	ID int64
	// Owner is the repository's owner; its ID is the OIDC token's
	// repository_owner_id.
	Owner Owner
}

// GetRepository reads owner/name (GET /repos/{owner}/{name}): its current
// full name, default branch, clone URL, numeric ids, and whether the
// authenticated developer has push access to it. Adopt uses this to
// validate --repo (existence, owner, permission) and to know which branch
// to clone; Adopt and iidp app bind record its ids.
func (c *Client) GetRepository(ctx context.Context, owner, name string) (Repository, error) {
	var repo struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		CloneURL      string `json:"clone_url"`
		ID            int64  `json:"id"`
		Owner         Owner  `json:"owner"`
		Permissions   struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if err := c.do(ctx, http.MethodGet, "/repos/"+owner+"/"+name, nil, &repo); err != nil {
		return Repository{}, fmt.Errorf("reading %s/%s: %w", owner, name, err)
	}
	return Repository{
		FullName:      repo.FullName,
		DefaultBranch: repo.DefaultBranch,
		CloneURL:      repo.CloneURL,
		CanPush:       repo.Permissions.Push,
		ID:            repo.ID,
		Owner:         repo.Owner,
	}, nil
}

// BranchExists reports whether branch already exists on owner/name
// (GET /repos/{owner}/{name}/branches/{branch}). Adopt uses this to refuse
// rather than push over a branch a previous run (or anything else) left
// behind.
func (c *Client) BranchExists(ctx context.Context, owner, name, branch string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/repos/"+owner+"/"+name+"/branches/"+branch, nil, nil)
	switch {
	case err == nil:
		return true, nil
	case IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("checking whether branch %s exists on %s/%s: %w", branch, owner, name, err)
	}
}

// PullRequest is what CreatePullRequest needs to open one.
type PullRequest struct {
	Title string
	Head  string
	Base  string
	Body  string
}

// CreatePullRequest opens a pull request on owner/name
// (POST /repos/{owner}/{name}/pulls) and returns its web ("html") URL.
func (c *Client) CreatePullRequest(ctx context.Context, owner, name string, pr PullRequest) (string, error) {
	body := map[string]any{
		"title": pr.Title,
		"head":  pr.Head,
		"base":  pr.Base,
		"body":  pr.Body,
	}
	var resp struct {
		HTMLURL string `json:"html_url"`
	}
	if err := c.do(ctx, http.MethodPost, "/repos/"+owner+"/"+name+"/pulls", body, &resp); err != nil {
		return "", fmt.Errorf("opening a pull request on %s/%s: %w", owner, name, err)
	}
	if resp.HTMLURL == "" {
		return "", fmt.Errorf("opening a pull request on %s/%s: GitHub returned no html_url", owner, name)
	}
	return resp.HTMLURL, nil
}

// TokenScopes is what GitHub reports about the OAuth scopes of the token a
// Client authenticates with
// (docs/implementation-notes/47-workflow-scope.md).
type TokenScopes struct {
	// Known is false when GitHub reported no scopes at all: fine-grained
	// personal access tokens and GitHub App tokens carry permissions
	// rather than scopes, and GitHub sends no X-OAuth-Scopes header for
	// them, so what they may push cannot be read up front.
	Known bool
	// Scopes are the token's scopes as GitHub listed them. Empty with
	// Known true is a classic token with no scopes at all.
	Scopes []string
}

// Has reports whether s lists scope. It is always false when Known is
// false.
func (s TokenScopes) Has(scope string) bool {
	return slices.Contains(s.Scopes, scope)
}

// TokenScopes reads the token's OAuth scopes from the X-OAuth-Scopes
// header GitHub sends on every authenticated response to a classic OAuth
// or personal access token (gh auth login's own token is one), here from
// the API root (GET /), which answers any token. gh auth status reads the
// same header the same way.
func (c *Client) TokenScopes(ctx context.Context) (TokenScopes, error) {
	header, err := c.send(ctx, http.MethodGet, "/", nil, nil)
	if err != nil {
		return TokenScopes{}, fmt.Errorf("reading your GitHub token's scopes: %w", err)
	}
	values, ok := header[http.CanonicalHeaderKey("X-OAuth-Scopes")]
	if !ok {
		return TokenScopes{}, nil
	}
	scopes := TokenScopes{Known: true}
	for _, v := range values {
		for _, scope := range strings.Split(v, ",") {
			if scope = strings.TrimSpace(scope); scope != "" {
				scopes.Scopes = append(scopes.Scopes, scope)
			}
		}
	}
	return scopes, nil
}

// AppSlug reads the authenticated GitHub App's slug (GET /app), the name
// its bot account is derived from: <slug>[bot]. Token must be a JWT signed
// with the App's private key.
func (c *Client) AppSlug(ctx context.Context) (string, error) {
	var app struct {
		Slug string `json:"slug"`
	}
	if err := c.do(ctx, http.MethodGet, "/app", nil, &app); err != nil {
		return "", fmt.Errorf("reading the GitHub App: %w", err)
	}
	if app.Slug == "" {
		return "", errors.New("reading the GitHub App: GitHub returned no slug")
	}
	return app.Slug, nil
}

// UserID reads the numeric id of the account login (GET /users/{login}).
// The Deploy gate uses it for the App's bot account, <slug>[bot], whose id
// is part of the noreply address GitHub links commits to.
func (c *Client) UserID(ctx context.Context, login string) (int64, error) {
	var user struct {
		ID int64 `json:"id"`
	}
	if err := c.do(ctx, http.MethodGet, "/users/"+url.PathEscape(login), nil, &user); err != nil {
		return 0, fmt.Errorf("reading the GitHub account %s: %w", login, err)
	}
	if user.ID == 0 {
		return 0, fmt.Errorf("reading the GitHub account %s: GitHub returned no id", login)
	}
	return user.ID, nil
}

// CreateInstallationToken creates an installation access token for
// installationID (POST /app/installations/{installation_id}/access_tokens).
// Token must be a JWT signed with the GitHub App's private key
// (internal/githubapp.SignJWT): this is the one request in this client
// authenticated as the App itself rather than as a user or an installation.
// Installation tokens expire one hour after creation.
func (c *Client) CreateInstallationToken(ctx context.Context, installationID int64) (string, error) {
	var resp struct {
		Token string `json:"token"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	if err := c.do(ctx, http.MethodPost, path, nil, &resp); err != nil {
		return "", fmt.Errorf("creating a GitHub App installation access token: %w", err)
	}
	if resp.Token == "" {
		return "", errors.New("creating a GitHub App installation access token: GitHub returned no token")
	}
	return resp.Token, nil
}

func (c *Client) baseURL() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return c.BaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient == nil {
		return http.DefaultClient
	}
	return c.HTTPClient
}

// do sends a request to path and, on a 2xx response with a non-empty body,
// decodes it into respBody. A non-2xx response is returned as a
// *statusError; callers match it with IsNotFound.
func (c *Client) do(ctx context.Context, method, path string, reqBody, respBody any) error {
	_, err := c.send(ctx, method, path, reqBody, respBody)
	return err
}

// send is do, also returning the response's headers for the one caller
// that reads them (TokenScopes).
func (c *Client) send(ctx context.Context, method, path string, reqBody, respBody any) (http.Header, error) {
	var r io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.Header, &statusError{status: resp.StatusCode, body: string(bytes.TrimSpace(data))}
	}
	if respBody != nil && len(data) > 0 {
		if err := json.Unmarshal(data, respBody); err != nil {
			return resp.Header, fmt.Errorf("decoding response from %s %s: %w", method, path, err)
		}
	}
	return resp.Header, nil
}
