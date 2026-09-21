// Package github holds what the CLI needs from GitHub outside git: today
// only the token the developer's gh login stored.
package github

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// TokenSource yields the GitHub token the CLI authenticates with. The
// production implementation asks the gh CLI; tests inject a fake.
type TokenSource interface {
	Token() (string, error)
}

// GhCLI reads the token stored by gh auth login, so the CLI needs no
// credential of its own.
type GhCLI struct{}

// Token runs gh auth token and returns the token it prints.
func (GhCLI) Token() (string, error) {
	cmd := exec.Command("gh", "auth", "token")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("the gh CLI is not installed; install it from https://cli.github.com and run gh auth login")
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("gh auth token: %s", msg)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("gh auth token printed no token; run gh auth login")
	}
	return token, nil
}
