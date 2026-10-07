package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// selfSignedCertificate returns a certificate for host, valid for a day, as
// PEM, and its DER bytes for comparing with what a server presents.
func selfSignedCertificate(host string) (certPEM, keyPEM, der []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return nil, nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err = x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, der, nil
}

// CheckCertificateServedForCallback proves Traefik presents a custom
// domain's certificate, whose Secret is in the Environment's namespace, on
// https://host/oauth2/callback, which an Ingress in the oauth2-proxy
// namespace routes and which names no secret. The connection sends host as
// SNI, as a browser does.
//
// kind can issue no certificate, so it writes a self-signed one into every
// Secret the host's own Ingress names, as cert-manager would. Traefik stops
// loading an Ingress's certificates at the first Secret that does not
// exist, so a Secret missing there would hide the host's.
func (c *Cluster) CheckCertificateServedForCallback(ctx context.Context, namespace, ingress, host string, timeout time.Duration) error {
	out, err := c.Kubectl(ctx, "-n", namespace, "get", "ingress", ingress, "-o", `jsonpath={range .spec.tls[*]}{.secretName} {.hosts[0]}{"\n"}{end}`)
	if err != nil {
		return fmt.Errorf("TLS entries of Ingress %s/%s: %w\n%s", namespace, ingress, err, out)
	}
	var want []byte
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		secret, tlsHost, ok := strings.Cut(line, " ")
		if !ok {
			return fmt.Errorf("Ingress %s/%s has a TLS entry without a secret or a host: %q", namespace, ingress, line)
		}
		der, err := c.writeSelfSignedSecret(ctx, namespace, secret, tlsHost)
		if err != nil {
			return err
		}
		if tlsHost == host {
			want = der
		}
	}
	if want == nil {
		return fmt.Errorf("Ingress %s/%s names no TLS Secret for %s", namespace, ingress, host)
	}
	return c.checkServedCertificate(ctx, host, "/oauth2/callback", want, timeout)
}

// writeSelfSignedSecret writes a self-signed certificate for host into the
// TLS Secret namespace/secret, returning its DER bytes.
func (c *Cluster) writeSelfSignedSecret(ctx context.Context, namespace, secret, host string) ([]byte, error) {
	certPEM, keyPEM, der, err := selfSignedCertificate(host)
	if err != nil {
		return nil, err
	}
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: kubernetes.io/tls
data:
  tls.crt: %s
  tls.key: %s
`, secret, namespace, base64.StdEncoding.EncodeToString(certPEM), base64.StdEncoding.EncodeToString(keyPEM))
	if err := c.Apply(ctx, manifest); err != nil {
		return nil, fmt.Errorf("writing the TLS Secret %s/%s: %w", namespace, secret, err)
	}
	return der, nil
}

// checkServedCertificate GETs path on host through Traefik's websecure
// entrypoint, with host as SNI, until the certificate presented is want and
// the callback is answered as loginCallbackAnswer requires.
func (c *Cluster) checkServedCertificate(ctx context.Context, host, path string, want []byte, timeout time.Duration) error {
	url := fmt.Sprintf("https://127.0.0.1:%d%s", c.HTTPSPort, path)
	client := &http.Client{
		Timeout: 10 * time.Second,
		// A new connection for every poll: a reused one keeps the
		// certificate of its first handshake, from before Traefik loaded
		// the Secret.
		Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{ServerName: host, InsecureSkipVerify: true}, //nolint:gosec // the certificate is compared byte for byte below
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var lastErr error
	return pollUntil(ctx, timeout, 3*time.Second,
		func() (bool, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return false, err
			}
			req.Host = host
			resp, err := client.Do(req)
			if err != nil {
				lastErr = fmt.Errorf("GET %s (SNI and Host: %s): %w", url, host, err)
				return false, nil
			}
			resp.Body.Close()
			if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 || !bytes.Equal(resp.TLS.PeerCertificates[0].Raw, want) {
				// Traefik loads a new Secret on its next configuration
				// reload; until then it presents its default certificate.
				presented := "none"
				if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
					presented = resp.TLS.PeerCertificates[0].Subject.String()
				}
				lastErr = fmt.Errorf("GET %s (SNI and Host: %s): presented a certificate other than %s's (%s)", url, host, host, presented)
				return false, nil
			}
			ok, retry, message := loginCallbackAnswer(resp)
			if !ok {
				err := fmt.Errorf("GET %s (SNI and Host: %s): %s", url, host, message)
				if retry {
					lastErr = err
					return false, nil
				}
				return false, err
			}
			c.Log("GET %s (SNI and Host: %s): %s, with %s's own certificate", url, host, message, host)
			return true, nil
		},
		func() error { return lastErr })
}
