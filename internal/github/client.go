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

	"github.com/Itema-as/iidp/internal/render"
)

// DefaultBaseURL is the GitHub REST API root.
const DefaultBaseURL = "https://api.github.com"

// Client is a small GitHub REST API client for what iidp needs beyond git.
type Client struct {
	// BaseURL is the API root. Empty means DefaultBaseURL.
	BaseURL string
	// Token authenticates every request: the developer's token in the CLI,
	// the GitHub App's JWT or installation token in the Deploy gate.
	Token string
	// HTTPClient makes the requests. Nil means http.DefaultClient.
	HTTPClient *http.Client
}

// NewClient makes a Client for the real GitHub API.
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
// a repository that does not exist, or that the token cannot see.
func IsNotFound(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.status == http.StatusNotFound
}

// RepositoryExists reports whether owner/name already exists.
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

// CreatedRepository is what CreateRepository reads back from GitHub.
type CreatedRepository struct {
	CloneURL string
	// DefaultBranch is the default branch GitHub gave the repository, so
	// the caller can tell whether SetDefaultBranch still needs to run.
	DefaultBranch string
	// ID is the repository's numeric id: the OIDC token's repository_id,
	// which the Platform binds the Application to.
	ID int64
	// Owner is the org the repository was created in; its ID is the OIDC
	// token's repository_owner_id.
	Owner Owner
}

// CreateRepository creates an empty repository named name in org.
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

// SetDefaultBranch sets owner/name's default branch, for a repository
// created with a different default, or none.
func (c *Client) SetDefaultBranch(ctx context.Context, owner, name, branch string) error {
	body := map[string]any{"default_branch": branch}
	if err := c.do(ctx, http.MethodPatch, "/repos/"+owner+"/"+name, body, nil); err != nil {
		return fmt.Errorf("setting the default branch of %s/%s to %s: %w", owner, name, branch, err)
	}
	return nil
}

// Repository is what GetRepository reads about an existing repository.
type Repository struct {
	// FullName is owner/name as GitHub reports it now, which differs from
	// what was asked for after a rename or transfer.
	FullName      string
	DefaultBranch string
	CloneURL      string
	// CanPush is whether the authenticated token may push.
	CanPush bool
	// ID is the repository's numeric id, the OIDC token's repository_id.
	ID int64
	// Owner is the repository's owner; its ID is the OIDC token's
	// repository_owner_id.
	Owner Owner
}

// GetRepository reads owner/name as the authenticated developer sees it.
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

// ReadableRepository is what RepositoryByID reads: the repository's
// current name and the token's permissions on it. GitHub's triage
// permission is not kept: it only ever comes with pull.
type ReadableRepository struct {
	FullName    string
	CanPull     bool
	CanPush     bool
	CanMaintain bool
	CanAdmin    bool
}

// Permission is the token's highest permission on the repository, in the
// words of the database access levels: render.AccessAdmin, AccessMaintain,
// AccessPush or AccessPull, or "" for none.
func (r ReadableRepository) Permission() string {
	switch {
	case r.CanAdmin:
		return render.AccessAdmin
	case r.CanMaintain:
		return render.AccessMaintain
	case r.CanPush:
		return render.AccessPush
	case r.CanPull:
		return render.AccessPull
	}
	return ""
}

// RepositoryByID reads a repository by its numeric id, which survives
// renames and transfers. For a private repository the user cannot see,
// GitHub answers 404 rather than 403.
func (c *Client) RepositoryByID(ctx context.Context, id int64) (ReadableRepository, error) {
	var repo struct {
		FullName    string `json:"full_name"`
		Permissions struct {
			Pull     bool `json:"pull"`
			Push     bool `json:"push"`
			Maintain bool `json:"maintain"`
			Admin    bool `json:"admin"`
		} `json:"permissions"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repositories/%d", id), nil, &repo); err != nil {
		return ReadableRepository{}, fmt.Errorf("reading repository id %d: %w", id, err)
	}
	return ReadableRepository{
		FullName:    repo.FullName,
		CanPull:     repo.Permissions.Pull,
		CanPush:     repo.Permissions.Push,
		CanMaintain: repo.Permissions.Maintain,
		CanAdmin:    repo.Permissions.Admin,
	}, nil
}

// AuthenticatedLogin reads the login of the user the token belongs to.
func (c *Client) AuthenticatedLogin(ctx context.Context) (string, error) {
	var user struct {
		Login string `json:"login"`
	}
	if err := c.do(ctx, http.MethodGet, "/user", nil, &user); err != nil {
		return "", fmt.Errorf("reading the GitHub account of your token: %w", err)
	}
	if user.Login == "" {
		return "", errors.New("reading the GitHub account of your token: GitHub returned no login")
	}
	return user.Login, nil
}

// IsUnauthorized reports whether err is GitHub's 401: a token it does not
// accept (expired, revoked or malformed).
func IsUnauthorized(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.status == http.StatusUnauthorized
}

// BranchExists reports whether branch already exists on owner/name.
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

// CreatePullRequest opens a pull request on owner/name and returns its web URL.
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

// TokenScopes is what GitHub reports about the OAuth scopes of a Client's
// token.
type TokenScopes struct {
	// Known is false for fine-grained personal access tokens and GitHub App
	// tokens: they carry permissions rather than scopes, and GitHub sends no
	// X-OAuth-Scopes header for them.
	Known  bool
	Scopes []string
}

// Has reports whether s lists scope.
func (s TokenScopes) Has(scope string) bool {
	return slices.Contains(s.Scopes, scope)
}

// TokenScopes reads the token's OAuth scopes from the X-OAuth-Scopes header
// on a request to the API root, which answers any token. gh auth status does
// the same.
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

// UserID reads the numeric id of the account login.
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

// CreateInstallationToken creates an installation access token, valid for
// an hour, for installationID. Token must be a JWT signed with the GitHub
// App's private key (githubapp.SignJWT).
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

// do sends a request to path and decodes a 2xx response's body into
// respBody. A non-2xx response is returned as a *statusError.
func (c *Client) do(ctx context.Context, method, path string, reqBody, respBody any) error {
	_, err := c.send(ctx, method, path, reqBody, respBody)
	return err
}

// send is do, also returning the response's headers.
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
