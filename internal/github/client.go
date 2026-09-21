package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// DefaultBaseURL is the GitHub REST API tests point elsewhere.
const DefaultBaseURL = "https://api.github.com"

// Client is a small GitHub REST API client for what the CLI needs beyond
// git: finding the developer's personal login, creating the Application
// repository and setting its default branch. BaseURL and HTTPClient are
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
// is true) or under owner's personal account, and returns its clone URL.
// It sets auto_init to false: Create always pushes its own first commit.
func (c *Client) CreateRepository(ctx context.Context, owner string, org bool, name string, private bool) (cloneURL string, err error) {
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
		CloneURL string `json:"clone_url"`
	}
	if err := c.do(ctx, http.MethodPost, path, body, &repo); err != nil {
		return "", fmt.Errorf("creating %s/%s: %w", owner, name, err)
	}
	if repo.CloneURL == "" {
		return "", fmt.Errorf("creating %s/%s: GitHub returned no clone_url", owner, name)
	}
	return repo.CloneURL, nil
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
	var r io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &statusError{status: resp.StatusCode, body: string(bytes.TrimSpace(data))}
	}
	if respBody != nil && len(data) > 0 {
		if err := json.Unmarshal(data, respBody); err != nil {
			return fmt.Errorf("decoding response from %s %s: %w", method, path, err)
		}
	}
	return nil
}
