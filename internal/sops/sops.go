// Package sops encrypts Kubernetes Secret documents for the Platform's KSOPS
// layout by shelling out to the sops binary, the way internal/git shells out
// to git and internal/github to gh.
//
// github.com/getsops/sops/v3 (even scoped to just its age and aes packages)
// pulls the AWS, GCP, Azure and HashiCorp Vault SDKs into the build, which is
// disproportionate for this CLI; see
// docs/implementation-notes/16-cli-secret-set.md for the numbers and the
// decision.
package sops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Encryptor encrypts a plaintext Kubernetes manifest with age, producing the
// document shape ksops expects: only data/stringData encrypted, an
// unencrypted sops metadata block naming the recipient. Production code uses
// Binary; tests can inject a fake.
type Encryptor interface {
	// Encrypt encrypts plaintext for recipient (an age public key) and
	// returns the encrypted document. filename is the logical file name
	// sops reports the document as; it is never read from or written to
	// disk. plaintext is never written to disk either: it travels to sops
	// over stdin and the encrypted result comes back over stdout.
	Encrypt(ctx context.Context, plaintext []byte, recipient, filename string) ([]byte, error)
}

// Binary shells out to the sops binary on PATH.
type Binary struct{}

// Available reports whether the sops binary can be found, so tests (and one
// day a preflight check) can give a clear message instead of a raw exec
// error.
func Available() bool {
	_, err := exec.LookPath("sops")
	return err == nil
}

// Encrypt implements Encryptor.
func (Binary) Encrypt(ctx context.Context, plaintext []byte, recipient, filename string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sops",
		"--encrypt",
		"--input-type", "yaml",
		"--output-type", "yaml",
		"--age", recipient,
		"--encrypted-regex", `^(data|stringData)$`,
		"--filename-override", filename,
		"/dev/stdin",
	)
	cmd.Stdin = bytes.NewReader(plaintext)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New("the sops binary is not installed or not on PATH; install it from https://github.com/getsops/sops")
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("sops --encrypt: %s", msg)
	}
	return out.Bytes(), nil
}
