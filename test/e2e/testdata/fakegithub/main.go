// Command fakegithub stands in for GitHub inside the kind harness, for the
// Deploy gate (docs/implementation-notes/60-deploy-gate.md): kind has no
// route to GitHub, and a real GitHub Actions OIDC token can only come from
// a real workflow run. It serves
//
//   - the OIDC issuer's key set, GET /.well-known/jwks, from the file
//     JWKS_FILE: the public half of the key the test signs its tokens
//     with, so the gate verifies them exactly as it verifies GitHub's;
//   - under /api, the three GitHub REST calls the gate makes: an
//     installation token (checking the App JWT against the public key in
//     APP_PUBLIC_KEY_FILE), the App's slug, and its bot account's id.
//
// test/e2e/deploygate.go builds it, loads it into the cluster and runs it;
// it is never published. It sits under testdata so that the pattern
// ./test/e2e/... the e2e workflow runs still matches one package, whose
// output go test then streams as the run goes instead of buffering it.
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
	"strings"
)

// The values the fake answers with; test/e2e checks the gate's commit
// against them.
const (
	InstallationToken = "e2e-installation-token"
	AppSlug           = "iidp-deploy"
	BotUserID         = 41898282
)

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
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	log.Print("fakegithub listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", logRequests(mux)))
}

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

// verifyAppJWT checks the request is signed by the App's private key, the
// way GitHub does.
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
