package githubapp_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/githubapp"
)

// testKey generates a fresh RSA key pair and PEM-encodes the private key
// the way GitHub's own "Generate a private key" button would (PKCS#1).
func testKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return key, pem.EncodeToMemory(block)
}

func TestSignJWTProducesATokenVerifiableWithThePublicKey(t *testing.T) {
	key, keyPEM := testKey(t)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	token, err := githubapp.SignJWT(12345, keyPEM, now)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3 (header.payload.signature)", len(parts))
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decoding header: %v", err)
	}
	var headerFields struct{ Alg, Typ string }
	if err := json.Unmarshal(header, &headerFields); err != nil {
		t.Fatal(err)
	}
	if headerFields.Alg != "RS256" || headerFields.Typ != "JWT" {
		t.Errorf("header = %+v, want alg RS256, typ JWT", headerFields)
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	var claims struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != strconv.Itoa(12345) {
		t.Errorf("iss = %q, want %q", claims.Iss, "12345")
	}
	if want := now.Add(-60 * time.Second).Unix(); claims.Iat != want {
		t.Errorf("iat = %d, want %d (60s in the past)", claims.Iat, want)
	}
	if want := now.Add(10 * time.Minute).Unix(); claims.Exp != want {
		t.Errorf("exp = %d, want %d (10 minutes ahead)", claims.Exp, want)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decoding signature: %v", err)
	}
	signingInput := parts[0] + "." + parts[1]
	hashed := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, hashed[:], sig); err != nil {
		t.Errorf("signature does not verify with the matching public key: %v", err)
	}
}

func TestSignJWTRejectsAnUnparsablePrivateKey(t *testing.T) {
	_, err := githubapp.SignJWT(1, []byte("not a PEM key"), time.Now())
	if err == nil {
		t.Fatal("err = nil, want an error for an unparsable key")
	}
}
