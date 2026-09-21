// Package githubapp signs the short-lived JSON Web Token a GitHub App
// authenticates with, and reads the App's private key from the
// environment. iidp ci set-image is the one caller: it mints a GitHub App
// installation token to write the Platform repository, following GitHub's
// documented shape for "Authenticating as a GitHub App" (checked against
// current GitHub REST API documentation; see
// docs/implementation-notes/12-deploy-workflow.md). No JWT library is used:
// the standard library's crypto/rsa, crypto/sha256, encoding/base64 and
// encoding/json are enough for the one algorithm GitHub requires, RS256.
package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// PrivateKeyEnvVar and PrivateKeyFileEnvVar are the environment variables
// iidp ci set-image reads the GitHub App's private key from: the PEM
// itself, or a path to a file holding it. The bootstrap wizard writes the
// PEM into the org Actions secret named after PrivateKeyEnvVar
// (docs/implementation-notes/05-bootstrap-wizard.md).
const (
	PrivateKeyEnvVar     = "IIDP_DEPLOY_APP_PRIVATE_KEY"
	PrivateKeyFileEnvVar = "IIDP_DEPLOY_APP_PRIVATE_KEY_FILE"
)

// AppIDEnvVar is the environment variable iidp ci set-image reads the
// GitHub App's id from: the org Actions variable the bootstrap wizard
// creates (docs/implementation-notes/05-bootstrap-wizard.md), passed to
// the deploy workflow as IIDP_DEPLOY_APP_ID: ${{ vars.IIDP_DEPLOY_APP_ID }}.
// Read from the environment rather than platform.yaml so minting a
// credential never depends on already having one to read the Platform
// repository with (docs/implementation-notes/12-deploy-workflow.md).
const AppIDEnvVar = "IIDP_DEPLOY_APP_ID"

// AppIDFromEnv reads and parses AppIDEnvVar.
func AppIDFromEnv() (int64, error) {
	v := os.Getenv(AppIDEnvVar)
	if v == "" {
		return 0, fmt.Errorf("%s is not set; iidp ci set-image cannot authenticate as the GitHub App without it", AppIDEnvVar)
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a valid integer: %w", AppIDEnvVar, v, err)
	}
	return id, nil
}

// PrivateKeyFromEnv reads the GitHub App's private key PEM from
// PrivateKeyEnvVar, or from the file named by PrivateKeyFileEnvVar when
// the former is not set. It is an error for neither to be set.
func PrivateKeyFromEnv() ([]byte, error) {
	if pem := os.Getenv(PrivateKeyEnvVar); pem != "" {
		return []byte(pem), nil
	}
	if path := os.Getenv(PrivateKeyFileEnvVar); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", PrivateKeyFileEnvVar, err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("neither %s nor %s is set; iidp ci set-image cannot authenticate as the GitHub App without the private key", PrivateKeyEnvVar, PrivateKeyFileEnvVar)
}

// SignJWT signs a GitHub App authentication JWT for appID with
// privateKeyPEM (PKCS#1 or PKCS#8, RSA), following GitHub's documented
// claims: iat 60 seconds in the past (GitHub's recommendation, to tolerate
// clock drift between this machine and GitHub's), exp 10 minutes ahead
// (GitHub's maximum), iss the App's id. now is a parameter so tests are
// deterministic.
func SignJWT(appID int64, privateKeyPEM []byte, now time.Time) (string, error) {
	key, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return "", err
	}
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(10 * time.Minute).Unix(),
		"iss": strconv.FormatInt(appID, 10),
	})
	if err != nil {
		return "", err
	}
	signingInput := base64URL(header) + "." + base64URL(claims)
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("signing the GitHub App JWT: %w", err)
	}
	return signingInput + "." + base64URL(sig), nil
}

func base64URL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// parsePrivateKey accepts either PKCS#1 ("RSA PRIVATE KEY", what GitHub's
// own "Generate a private key" button downloads) or PKCS#8
// ("PRIVATE KEY") PEM encodings of an RSA key.
func parsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("the GitHub App private key is not PEM-encoded")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing the GitHub App private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the GitHub App private key is not RSA")
	}
	return key, nil
}
