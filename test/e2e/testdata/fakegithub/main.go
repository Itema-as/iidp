// Command fakegithub stands in for GitHub inside the kind harness, which has
// no route to GitHub and no real GitHub Actions OIDC tokens. It serves:
//
//   - GET /.well-known/jwks: the key set the test signs its tokens with;
//   - under /api, the GitHub REST calls the Deploy gate makes, checking the
//     App JWT against APP_PUBLIC_KEY_FILE;
//   - under /api/v3, GitHub Enterprise's API path, the calls ArgoCD's Pull
//     Request generator makes;
//   - GET /api/repositories/{id}, the access check made with a developer's
//     token;
//   - PUT /e2e/pulls/{owner}/{repo}, which the test calls to set the pull
//     requests.
//
// It sits under testdata so that ./test/e2e/... matches a single package,
// whose output go test streams instead of buffering.
package main

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The values the fake answers with; test/e2e checks the gate's commit
// against them.
const (
	InstallationToken = "e2e-installation-token"
	AppSlug           = "iidp-deploy"
	BotUserID         = 41898282

	// DeveloperToken is the gh auth token test/e2e sends to the gate's
	// status endpoint.
	DeveloperToken = "e2e-developer-token"
)

// readableRepositories are the repository ids DeveloperToken may read:
// shop's and notes's, but not brochure's.
var readableRepositories = map[string]string{
	"700000002": "Itema-as/shop",
	"700000003": "Itema-as/notes",
}

func main() {
	jwks, err := os.ReadFile(os.Getenv("JWKS_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	appKey, err := readPublicKey(os.Getenv("APP_PUBLIC_KEY_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	})
	mux.HandleFunc("POST /api/app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if err := verifyAppJWT(r, appKey); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]string{"token": InstallationToken})
	})
	mux.HandleFunc("GET /api/app", func(w http.ResponseWriter, r *http.Request) {
		if err := verifyAppJWT(r, appKey); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"slug": AppSlug})
	})
	mux.HandleFunc("GET /api/users/{login}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+InstallationToken || r.PathValue("login") != AppSlug+"[bot]" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"id": BotUserID, "login": AppSlug + "[bot]"})
	})
	// ArgoCD's generator mints its installation token with
	// ghinstallation, at <api>/app/installations/{id}/access_tokens.
	mux.HandleFunc("POST /api/v3/app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if err := verifyAppJWT(r, appKey); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"token": InstallationToken, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	})
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/pulls", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "token "+InstallationToken && auth != "Bearer "+InstallationToken {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		state := r.URL.Query().Get("state")
		if state == "" {
			state = "open"
		}
		pulls.Lock()
		defer pulls.Unlock()
		list := []pullRequest{}
		for _, pr := range pulls.byRepo[r.PathValue("owner")+"/"+r.PathValue("repo")] {
			if state == "all" || pr.State == state {
				list = append(list, pr)
			}
		}
		writeJSON(w, list)
	})
	mux.HandleFunc("PUT /e2e/pulls/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		var list []pullRequest
		if err := json.NewDecoder(r.Body).Decode(&list); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		pulls.Lock()
		pulls.byRepo[r.PathValue("owner")+"/"+r.PathValue("repo")] = list
		pulls.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	// GitHub answers 404 for a repository the token cannot read.
	mux.HandleFunc("GET /api/repositories/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+DeveloperToken {
			http.Error(w, `{"message": "Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		fullName, ok := readableRepositories[r.PathValue("id")]
		if !ok {
			http.Error(w, `{"message": "Not Found"}`, http.StatusNotFound)
			return
		}
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		writeJSON(w, map[string]any{"id": id, "full_name": fullName, "private": true,
			"permissions": map[string]bool{"pull": true}})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	log.Print("fakegithub listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", logRequests(mux)))
}

// pullRequest has every field ArgoCD's generator reads; it dereferences
// each one.
type pullRequest struct {
	Number int     `json:"number"`
	Title  string  `json:"title"`
	State  string  `json:"state"`
	Labels []label `json:"labels"`
	Head   ref     `json:"head"`
	Base   ref     `json:"base"`
	User   user    `json:"user"`
}

type label struct {
	Name string `json:"name"`
}

type ref struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type user struct {
	Login string `json:"login"`
}

var pulls = struct {
	sync.Mutex
	byRepo map[string][]pullRequest
}{byRepo: map[string][]pullRequest{}}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		h.ServeHTTP(w, r)
	})
}

func readPublicKey(path string) (*rsa.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("APP_PUBLIC_KEY_FILE is not PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("APP_PUBLIC_KEY_FILE is not an RSA key")
	}
	return rsaKey, nil
}

// verifyAppJWT checks the request is signed by the App's private key.
func verifyAppJWT(r *http.Request, key *rsa.PublicKey) error {
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return errors.New("no bearer JWT")
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return errors.New("not a JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	hashed := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, hashed[:], sig)
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
