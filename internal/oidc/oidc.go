// Package oidc verifies the GitHub Actions OIDC tokens the Deploy gate is
// called with: an RS256 JSON Web Token signed by a key the issuer publishes
// in its JSON Web Key Set, for the gate's own audience, not expired. Only
// the standard library is used, in keeping with internal/githubapp: RS256
// is the one algorithm GitHub signs with, and the claims the gate reads are
// a handful of strings (docs/implementation-notes/60-deploy-gate.md).
package oidc

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitHubActionsIssuer is the issuer of GitHub Actions OIDC tokens, and
// GitHubActionsJWKSURL where it publishes its signing keys.
const (
	GitHubActionsIssuer  = "https://token.actions.githubusercontent.com"
	GitHubActionsJWKSURL = GitHubActionsIssuer + "/.well-known/jwks"
)

// ErrInvalidToken is wrapped by every refusal of Verify: the token is not
// one the gate can trust, whatever it claims.
var ErrInvalidToken = errors.New("invalid GitHub Actions OIDC token")

// leeway is how far the gate's clock may disagree with the issuer's when
// checking exp, nbf and iat.
const leeway = time.Minute

// keysMaxAge is how long a fetched key set is used before it is fetched
// again; minRefetch is the least time between two fetches triggered by a
// token signed with a key the gate does not know yet, so a stream of
// forged kids cannot make the gate hammer the issuer.
const (
	keysMaxAge = time.Hour
	minRefetch = 30 * time.Second
)

// Claims are the claims of a GitHub Actions OIDC token the gate reads. The
// numeric ids arrive as decimal strings; they are kept as strings and
// compared with strconv.FormatInt, as docs/platform-repository.md says.
type Claims struct {
	Issuer            string   `json:"iss"`
	Audience          audience `json:"aud"`
	Expiry            int64    `json:"exp"`
	NotBefore         int64    `json:"nbf"`
	IssuedAt          int64    `json:"iat"`
	Repository        string   `json:"repository"`
	RepositoryID      idString `json:"repository_id"`
	RepositoryOwner   string   `json:"repository_owner"`
	RepositoryOwnerID idString `json:"repository_owner_id"`
	Ref               string   `json:"ref"`
	RefType           string   `json:"ref_type"`
	EventName         string   `json:"event_name"`
	Actor             string   `json:"actor"`
	ActorID           idString `json:"actor_id"`
	SHA               string   `json:"sha"`
	WorkflowRef       string   `json:"workflow_ref"`
	RunID             idString `json:"run_id"`
}

// audience is the aud claim, which a JWT may carry as one string or as a
// list of them.
type audience []string

func (a *audience) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("aud is neither a string nor a list of strings")
	}
	*a = many
	return nil
}

// idString is a numeric id claim. GitHub sends them as decimal strings; a
// JSON number is accepted too and kept in its decimal form.
type idString string

func (s *idString) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		*s = idString(str)
		return nil
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return fmt.Errorf("not a string or a number: %s", data)
	}
	*s = idString(n.String())
	return nil
}

// Is reports whether the id claim is id.
func (s idString) Is(id int64) bool { return string(s) == strconv.FormatInt(id, 10) }

// String is the claim as sent.
func (s idString) String() string { return string(s) }

// Verifier checks tokens from one issuer for one audience.
type Verifier struct {
	// Issuer is the iss every token must carry.
	Issuer string
	// JWKSURL is where Issuer publishes its signing keys.
	JWKSURL string
	// Audience is the aud every token must include: the gate's own URL.
	Audience string
	// HTTPClient fetches the key set; nil means a client with a short
	// timeout.
	HTTPClient *http.Client
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// Verify checks token's signature against the issuer's keys, its issuer,
// audience and lifetime, and returns its claims. Every refusal wraps
// ErrInvalidToken and says what was wrong.
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, fmt.Errorf("%w: not a JSON Web Token", ErrInvalidToken)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return Claims{}, fmt.Errorf("%w: the header: %v", ErrInvalidToken, err)
	}
	if header.Alg != "RS256" {
		return Claims{}, fmt.Errorf("%w: signed with %q, want RS256", ErrInvalidToken, header.Alg)
	}
	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return Claims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: the signature is not base64url", ErrInvalidToken)
	}
	hashed := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, hashed[:], sig); err != nil {
		return Claims{}, fmt.Errorf("%w: the signature does not verify against %s's key %q", ErrInvalidToken, v.Issuer, header.Kid)
	}

	var claims Claims
	if err := decodeSegment(parts[1], &claims); err != nil {
		return Claims{}, fmt.Errorf("%w: the claims: %v", ErrInvalidToken, err)
	}
	if claims.Issuer != v.Issuer {
		return Claims{}, fmt.Errorf("%w: issued by %q, want %q", ErrInvalidToken, claims.Issuer, v.Issuer)
	}
	if !containsAudience(claims.Audience, v.Audience) {
		return Claims{}, fmt.Errorf("%w: its audience is %q, and the Deploy gate only accepts %q; request the token with audience %s", ErrInvalidToken, strings.Join(claims.Audience, ", "), v.Audience, v.Audience)
	}
	now := v.now()
	if claims.Expiry == 0 {
		return Claims{}, fmt.Errorf("%w: it has no expiry", ErrInvalidToken)
	}
	if exp := time.Unix(claims.Expiry, 0); now.After(exp.Add(leeway)) {
		return Claims{}, fmt.Errorf("%w: it expired at %s", ErrInvalidToken, exp.UTC().Format(time.RFC3339))
	}
	if claims.NotBefore != 0 {
		if nbf := time.Unix(claims.NotBefore, 0); now.Add(leeway).Before(nbf) {
			return Claims{}, fmt.Errorf("%w: it is not valid before %s", ErrInvalidToken, nbf.UTC().Format(time.RFC3339))
		}
	}
	if claims.IssuedAt != 0 {
		if iat := time.Unix(claims.IssuedAt, 0); now.Add(leeway).Before(iat) {
			return Claims{}, fmt.Errorf("%w: it was issued in the future, at %s", ErrInvalidToken, iat.UTC().Format(time.RFC3339))
		}
	}
	return claims, nil
}

// containsAudience matches the audience with or without a trailing slash,
// so https://deploy.example and https://deploy.example/ are one audience.
func containsAudience(auds audience, want string) bool {
	want = strings.TrimSuffix(want, "/")
	for _, aud := range auds {
		if strings.TrimSuffix(aud, "/") == want {
			return true
		}
	}
	return false
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// key returns the issuer's public key kid, fetching the key set when none
// is cached, when the cache is old, or when kid is not in it (the issuer
// rotated its keys) and the last fetch was long enough ago.
func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	stale := v.keys == nil || now.Sub(v.fetched) > keysMaxAge
	if key, ok := v.keys[kid]; ok && !stale {
		return key, nil
	}
	if stale || now.Sub(v.fetched) > minRefetch {
		keys, err := v.fetch(ctx)
		if err != nil {
			if key, ok := v.keys[kid]; ok {
				// The issuer is unreachable but the key was known: keep
				// using it rather than refusing every deploy.
				return key, nil
			}
			return nil, err
		}
		v.keys, v.fetched = keys, now
	}
	if key, ok := v.keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("%w: signed with key %q, which %s does not publish", ErrInvalidToken, kid, v.JWKSURL)
}

// fetch reads the JSON Web Key Set at JWKSURL and keeps its RSA keys.
func (v *Verifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	client := v.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.JWKSURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching the signing keys from %s: %w", v.JWKSURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("fetching the signing keys from %s: %w", v.JWKSURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching the signing keys from %s: HTTP %d", v.JWKSURL, resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("parsing the signing keys from %s: %w", v.JWKSURL, err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s publishes no RSA signing key", v.JWKSURL)
	}
	return keys, nil
}

func decodeSegment(segment string, into any) error {
	data, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return errors.New("not base64url")
	}
	return json.Unmarshal(data, into)
}
