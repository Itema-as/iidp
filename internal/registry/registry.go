// Package registry asks a container registry whether an image tag exists,
// the way a client about to pull it would: a HEAD on the tag's manifest,
// through the registry's bearer-token flow. The Deploy gate uses it to
// refuse a tag that was never pushed before committing it
// (docs/implementation-notes/61-image-check.md). Only the standard library
// is used, like internal/oidc: the protocol is two requests and a
// challenge header.
package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ManifestMediaTypes are what a manifest HEAD accepts: an image manifest or
// a multi-platform index, in both their OCI and their Docker forms. A
// registry answers 404 for a manifest whose type the client does not
// accept, so leaving out the index types would call a multi-platform
// image missing (GHCR and Docker Hub both serve indexes).
var ManifestMediaTypes = []string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}

// ErrNotFound means the registry says the tag does not exist.
var ErrNotFound = errors.New("no such image tag")

// ErrDenied means the registry refused the access the check needed. GHCR
// answers a package that does not exist exactly like one the credential
// may not read, so this may also mean the image was never pushed.
var ErrDenied = errors.New("the registry refused access")

// ErrUnavailable means the registry could not be asked: unreachable, timed
// out, a 5xx or an answer the check does not understand.
var ErrUnavailable = errors.New("the registry could not be asked")

// Credential is a username and password (for GHCR, a classic token) sent
// with basic auth to one registry host.
type Credential struct {
	Username string
	Password string
}

// Checker checks that image tags exist.
type Checker struct {
	// HTTPClient makes the requests; nil means http.DefaultClient. Timeout
	// bounds them either way.
	HTTPClient *http.Client
	// CredentialHost is the one registry host Credential is for (ghcr.io).
	// It is sent only to that host, and to a token realm on that host over
	// https, never to another registry or another realm, so an image
	// repository naming some other registry cannot collect it. Every other
	// registry is asked anonymously.
	CredentialHost string
	// Credential yields the credential for CredentialHost on each check,
	// so a rotated one is used without a restart. Nil means anonymous
	// everywhere.
	Credential func() (Credential, error)
	// Timeout bounds one whole check; zero means 30 seconds.
	Timeout time.Duration
}

// Image is a parsed image repository: the registry host its reference
// names, the host its API is served from, and the repository's name there.
type Image struct {
	// Host is the registry as the reference names it (ghcr.io, docker.io).
	Host string
	// APIHost is where its /v2/ API is: Host, except registry-1.docker.io
	// for Docker Hub.
	APIHost string
	// Name is the repository within the registry (itema-as/shop,
	// library/nginx).
	Name string
}

var (
	hostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]+)?$`)
	// namePattern is the distribution specification's repository name
	// grammar: lowercase path components separated by slashes.
	namePattern = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)
)

// ParseRepository splits an image repository (what values.yaml's
// image.repository holds, with no tag or digest) the way container
// engines do: a first component with a dot or a colon, or localhost, is a
// registry host, and anything else is on Docker Hub, where a one-component
// name is under library/.
func ParseRepository(repository string) (Image, error) {
	host, name := "docker.io", repository
	if first, rest, ok := strings.Cut(repository, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		host, name = first, rest
	}
	if !hostPattern.MatchString(host) || !namePattern.MatchString(name) {
		return Image{}, fmt.Errorf("%q is not an image repository (a registry host and a lowercase name, with no tag)", repository)
	}
	image := Image{Host: host, APIHost: host, Name: name}
	if host == "docker.io" || host == "index.docker.io" {
		image.APIHost = "registry-1.docker.io"
		if !strings.Contains(name, "/") {
			image.Name = "library/" + name
		}
	}
	return image, nil
}

// Exists checks that repository:tag exists: nil when it does, or an error
// wrapping ErrNotFound, ErrDenied or ErrUnavailable. tag must already be a
// valid tag; it is put into the URL path as is.
func (c *Checker) Exists(ctx context.Context, repository, tag string) error {
	image, err := ParseRepository(repository)
	if err != nil {
		return err
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	manifest := "https://" + image.APIHost + "/v2/" + image.Name + "/manifests/" + tag
	resp, err := c.head(ctx, manifest, "")
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		authorization, err := c.authorize(ctx, image, resp.Header.Get("WWW-Authenticate"))
		if err != nil {
			return err
		}
		if resp, err = c.head(ctx, manifest, authorization); err != nil {
			return err
		}
	}
	return outcome(image.Host, resp.StatusCode)
}

// head sends the manifest HEAD and returns the response, its body already
// closed; a transport failure is ErrUnavailable.
func (c *Checker) head(ctx context.Context, manifest, authorization string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, manifest, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", strings.Join(ManifestMediaTypes, ", "))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	resp.Body.Close()
	return resp, nil
}

// outcome turns the registry's answer to the manifest HEAD into Exists's
// result. A HEAD answer has no body, so the status is all there is.
func outcome(host string, status int) error {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s answered HTTP %d to the manifest request", ErrDenied, host, status)
	default:
		return fmt.Errorf("%w: %s answered HTTP %d to the manifest request", ErrUnavailable, host, status)
	}
}

// authorize answers a 401's challenge with the Authorization header for
// the retried HEAD: basic auth for a Basic challenge, or a bearer token
// from the challenge's realm for a Bearer one, asked for with the
// credential when it is for this registry and anonymously otherwise.
func (c *Checker) authorize(ctx context.Context, image Image, header string) (string, error) {
	scheme, params := parseChallenge(header)
	switch strings.ToLower(scheme) {
	case "basic":
		cred, ok, err := c.credentialFor(image.Host, nil)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("%w: %s asks for a password, and the Deploy gate has none for it", ErrDenied, image.Host)
		}
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(cred.Username+":"+cred.Password)), nil
	case "bearer":
	default:
		return "", fmt.Errorf("%w: %s answered 401 without a challenge the check understands (WWW-Authenticate: %q)", ErrUnavailable, image.Host, header)
	}

	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Host == "" || (realm.Scheme != "https" && realm.Scheme != "http") {
		return "", fmt.Errorf("%w: %s's token realm %q is not a URL", ErrUnavailable, image.Host, params["realm"])
	}
	query := realm.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	// Always the pull scope of this one repository, whatever the challenge
	// names: the check never asks for more than reading.
	query.Set("scope", "repository:"+image.Name+":pull")
	realm.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	cred, ok, err := c.credentialFor(image.Host, realm)
	if err != nil {
		return "", err
	}
	if ok {
		req.SetBasicAuth(cred.Username, cred.Password)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: asking %s for a token: %v", ErrUnavailable, realm.Host, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", fmt.Errorf("%w: %s refused a token (HTTP %d%s)", ErrDenied, realm.Host, resp.StatusCode, registryError(body))
	default:
		return "", fmt.Errorf("%w: %s answered HTTP %d to a token request%s", ErrUnavailable, realm.Host, resp.StatusCode, registryError(body))
	}
	var token struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &token); err != nil || (token.Token == "" && token.AccessToken == "") {
		return "", fmt.Errorf("%w: %s's token endpoint answered without a token", ErrUnavailable, realm.Host)
	}
	if token.Token == "" {
		token.Token = token.AccessToken
	}
	return "Bearer " + token.Token, nil
}

// credentialFor returns the credential when it may be sent: the registry
// is CredentialHost and, for a token realm, the realm is https on that
// same host.
func (c *Checker) credentialFor(host string, realm *url.URL) (Credential, bool, error) {
	if c.Credential == nil || c.CredentialHost == "" || !strings.EqualFold(host, c.CredentialHost) {
		return Credential{}, false, nil
	}
	if realm != nil && (realm.Scheme != "https" || !strings.EqualFold(realm.Host, c.CredentialHost)) {
		return Credential{}, false, nil
	}
	cred, err := c.Credential()
	if err != nil {
		return Credential{}, false, err
	}
	return cred, true, nil
}

func (c *Checker) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// registryError is ": " and the first error of a registry's JSON error
// body ({"errors": [{"code", "message"}]}), or "" when it is not one.
func registryError(body []byte) string {
	var e struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &e) != nil || len(e.Errors) == 0 {
		return ""
	}
	return ": " + strings.TrimSpace(e.Errors[0].Code+" "+e.Errors[0].Message)
}

// parseChallenge splits a WWW-Authenticate header into its scheme and its
// parameters (RFC 7235): Bearer realm="https://ghcr.io/token",
// service="ghcr.io",scope="repository:itema-as/shop:pull". A quoted value
// may hold commas (a scope with pull,push) and backslash escapes.
func parseChallenge(header string) (string, map[string]string) {
	header = strings.TrimSpace(header)
	scheme, rest, _ := strings.Cut(header, " ")
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		after = strings.TrimLeft(after, " ")
		var value strings.Builder
		if strings.HasPrefix(after, `"`) {
			i := 1
			for ; i < len(after) && after[i] != '"'; i++ {
				if after[i] == '\\' && i+1 < len(after) {
					i++
				}
				value.WriteByte(after[i])
			}
			after = after[min(i+1, len(after)):]
		} else {
			v, tail, _ := strings.Cut(after, ",")
			value.WriteString(strings.TrimSpace(v))
			after = "," + tail
		}
		params[key] = value.String()
		rest = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(after), ","))
	}
	return scheme, params
}
