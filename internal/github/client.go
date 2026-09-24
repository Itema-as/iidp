package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

// DefaultBaseURL is the GitHub REST API tests point elsewhere.
const DefaultBaseURL = "https://api.github.com"

// Client is a small GitHub REST API client for what the CLI needs beyond
// git: reading the token's scopes, finding the developer's personal login,
// creating the Application repository and setting its default branch. BaseURL and HTTPClient are
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

// CurrentUser returns the login of the developer the token belongs to
// (GET /user): the personal account Create uses when --owner user is
// chosen.
func (c *Client) CurrentUser(ctx context.Context) (string, error) {
	var user struct {
		Login string `json:"login"`
	}
	if err := c.do(ctx, http.MethodGet, "/user", nil, &user); err != nil {
		return "", fmt.Errorf("looking up your GitHub login: %w", err)
	}
	if user.Login == "" {
		return "", errors.New("looking up your GitHub login: GET /user returned no login")
	}
	return user.Login, nil
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

// CreateRepository creates a new repository named name, under org (when org
// is true) or under owner's personal account, and returns its clone URL and
// the default branch GitHub gave it (so the caller can tell whether
// SetDefaultBranch still needs to run). It sets auto_init to false: Create
// always pushes its own first commit.
func (c *Client) CreateRepository(ctx context.Context, owner string, org bool, name string, private bool) (cloneURL, defaultBranch string, err error) {
	path := "/user/repos"
	if org {
		path = "/orgs/" + owner + "/repos"
	}
	body := map[string]any{
		"name":      name,
		"private":   private,
		"auto_init": false,
	}
	var repo struct {
		CloneURL      string `json:"clone_url"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.do(ctx, http.MethodPost, path, body, &repo); err != nil {
		return "", "", fmt.Errorf("creating %s/%s: %w", owner, name, err)
	}
	if repo.CloneURL == "" {
		return "", "", fmt.Errorf("creating %s/%s: GitHub returned no clone_url", owner, name)
	}
	return repo.CloneURL, repo.DefaultBranch, nil
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
// fields the Adopt path needs (docs/implementation-notes/15-cli-adopt-path.md).
// CanPush is GitHub's own view of the authenticated token's permission,
// present in the response only because the request is authenticated.
type Repository struct {
	DefaultBranch string
	CloneURL      string
	CanPush       bool
}

// GetRepository reads owner/name (GET /repos/{owner}/{name}): its default
// branch, its clone URL, and whether the authenticated developer has push
// access to it. Adopt uses this both to validate --repo (existence,
// permission) and to know which branch to clone.
func (c *Client) GetRepository(ctx context.Context, owner, name string) (Repository, error) {
	var repo struct {
		DefaultBranch string `json:"default_branch"`
		CloneURL      string `json:"clone_url"`
		Permissions   struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if err := c.do(ctx, http.MethodGet, "/repos/"+owner+"/"+name, nil, &repo); err != nil {
		return Repository{}, fmt.Errorf("reading %s/%s: %w", owner, name, err)
	}
	return Repository{
		DefaultBranch: repo.DefaultBranch,
		CloneURL:      repo.CloneURL,
		CanPush:       repo.Permissions.Push,
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

// Installation is one GitHub App installation, as returned by
// GET /app/installations: only the fields iidp ci set-image needs to find
// the org's installation.
type Installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
	} `json:"account"`
}

// ListInstallations lists the installations of the authenticated GitHub
// App (GET /app/installations). Token must be a JWT signed with the App's
// private key, the same App-level authentication CreateInstallationToken
// uses. Confirmed against the current GitHub REST API documentation
// ("List installations for the authenticated app"): the response is a
// plain JSON array, unlike GET /user/installations' wrapped shape. Not
// paginated: an org's own deploy App is expected to have a handful of
// installations at most, well inside the default page size.
func (c *Client) ListInstallations(ctx context.Context) ([]Installation, error) {
	var installations []Installation
	if err := c.do(ctx, http.MethodGet, "/app/installations", nil, &installations); err != nil {
		return nil, fmt.Errorf("listing GitHub App installations: %w", err)
	}
	return installations, nil
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
