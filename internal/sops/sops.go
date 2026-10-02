// Package sops encrypts Kubernetes Secret documents for the Platform's KSOPS
// layout by shelling out to the sops binary.
//
// It does not import github.com/getsops/sops/v3: even its age and aes
// packages pull the AWS, GCP, Azure and Vault SDKs into the build.
package sops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Encryptor encrypts a plaintext Kubernetes manifest with age into the shape
// ksops expects: only data/stringData encrypted.
type Encryptor interface {
	// Encrypt encrypts plaintext for recipient (an age public key).
	// filename is only the name sops reports; neither it nor the plaintext
	// touches disk.
	Encrypt(ctx context.Context, plaintext []byte, recipient, filename string) ([]byte, error)
}

// Binary shells out to the sops binary on PATH.
type Binary struct{}

// Available reports whether the sops binary is on PATH.
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
