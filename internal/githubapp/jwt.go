// Package githubapp signs the short-lived JSON Web Token a GitHub App
// authenticates with. It uses no JWT library: the standard library is
// enough for RS256, the one algorithm GitHub requires.
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
	"strconv"
	"time"
)

// SignJWT signs a GitHub App authentication JWT for appID with
// privateKeyPEM (PKCS#1 or PKCS#8, RSA). iat is 60 seconds in the past to
// tolerate clock drift, as GitHub recommends; exp is GitHub's 10-minute
// maximum.
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

// parsePrivateKey accepts PKCS#1 (what GitHub's "Generate a private key"
// downloads) or PKCS#8 PEM encodings of an RSA key.
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
