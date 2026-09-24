// Package git wraps the git binary for the few operations the CLI needs on
// the Platform repository: a shallow clone, adding files, committing and
// pushing. It shells out rather than embedding a git implementation so the
// developer's own git configuration (identity, signing) applies to the
// commits the CLI makes.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ErrPushRejected is returned by Push when the remote branch has moved
// since the clone, so the push was not a fast-forward.
var ErrPushRejected = errors.New("the remote branch moved since the clone")

// Auth is the credential git presents to an HTTPS remote. An empty token
// means no credential, which is what a file:// or ssh remote needs.
// Identity, when set, is who the commits are made as, author and committer
// both; empty leaves that to the developer's own git configuration.
// Committer, when set, overrides the committer alone, so a commit can be
// authored by one account and committed by another: the Deploy gate
// commits a deploy authored by the GitHub user who asked for it and
// committed by the GitHub App that wrote it
// (docs/implementation-notes/60-deploy-gate.md).
type Auth struct {
	Token     string
	Identity  Identity
	Committer Identity
}

// Identity is a name and an email a commit is attributed to.
type Identity struct {
	Name  string
	Email string
}

func (id Identity) set() bool { return id.Name != "" && id.Email != "" }

// Repository is a working copy of a remote branch.
type Repository struct {
	Dir  string
	auth Auth
}

// Clone makes a shallow, single-branch clone of branch at url into dir.
func Clone(ctx context.Context, url, branch, dir string, auth Auth) (*Repository, error) {
	r := &Repository{Dir: dir, auth: auth}
	if _, err := r.run(ctx, "", "clone", "--quiet", "--depth", "1", "--single-branch", "--branch", branch, url, dir); err != nil {
		return nil, fmt.Errorf("clone %s: %w", url, err)
	}
	return r, nil
}

// Init creates a new repository at dir (which must already exist and hold
// the files to commit) on branch, with an origin remote set to url, ready
// for Add, Commit and Push. Create uses it for the Application repository's
// first commit, where there is nothing to clone yet.
func Init(ctx context.Context, dir, branch, url string, auth Auth) (*Repository, error) {
	r := &Repository{Dir: dir, auth: auth}
	if _, err := r.run(ctx, dir, "init", "--quiet", "--initial-branch", branch); err != nil {
		return nil, fmt.Errorf("init: %w", err)
	}
	if _, err := r.run(ctx, dir, "remote", "add", "origin", url); err != nil {
		return nil, fmt.Errorf("remote add origin %s: %w", url, err)
	}
	return r, nil
}

// Add stages the given paths, relative to the repository root.
func (r *Repository) Add(ctx context.Context, paths ...string) error {
	args := append([]string{"add", "--"}, paths...)
	if _, err := r.run(ctx, r.Dir, args...); err != nil {
		return fmt.Errorf("add: %w", err)
	}
	return nil
}

// Commit records the staged changes with the author from git config.
func (r *Repository) Commit(ctx context.Context, message string) error {
	if _, err := r.run(ctx, r.Dir, "commit", "--quiet", "--message", message); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// CreateBranch creates and checks out a new local branch named name from
// the current HEAD. Adopt uses it, after cloning the default branch, to
// switch to the branch it pushes the pull request from
// (docs/implementation-notes/15-cli-adopt-path.md), so the local branch
// name matches what is pushed even though Push itself always targets its
// branch argument regardless of the current branch.
func (r *Repository) CreateBranch(ctx context.Context, name string) error {
	if _, err := r.run(ctx, r.Dir, "checkout", "--quiet", "-b", name); err != nil {
		return fmt.Errorf("checkout -b %s: %w", name, err)
	}
	return nil
}

// Remove deletes the given paths (files or directories) from the working
// tree and stages the removal, the way "git rm -r" does. iidp app delete
// uses it to remove an Environment's whole directory in one step.
func (r *Repository) Remove(ctx context.Context, paths ...string) error {
	args := append([]string{"rm", "--quiet", "-r", "--"}, paths...)
	if _, err := r.run(ctx, r.Dir, args...); err != nil {
		return fmt.Errorf("rm: %w", err)
	}
	return nil
}

// Head is the commit id HEAD points at.
func (r *Repository) Head(ctx context.Context) (string, error) {
	out, err := r.run(ctx, r.Dir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// Push pushes HEAD to branch on origin. A push refused because the branch
// moved returns an error wrapping ErrPushRejected; every other refusal
// (permissions, network) is returned as is.
func (r *Repository) Push(ctx context.Context, branch string) error {
	out, err := r.run(ctx, r.Dir, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	if err == nil {
		return nil
	}
	if strings.Contains(out, "[rejected]") {
		return fmt.Errorf("push %s: %w", branch, ErrPushRejected)
	}
	return fmt.Errorf("push %s: %w", branch, err)
}

// run executes git with the repository's credential configured and never
// prompts: a missing or refused credential fails instead of hanging.
func (r *Repository) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append(r.configArgs(), args...)...)
	cmd.Dir = dir
	// LC_ALL=C keeps git's messages in English, which Push matches on.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if r.auth.Token != "" {
		cmd.Env = append(cmd.Env, "IIDP_GIT_TOKEN="+r.auth.Token)
	}
	// Later entries win over any identity already in the environment.
	if id := r.auth.Identity; id.set() {
		cmd.Env = append(cmd.Env,
			"GIT_AUTHOR_NAME="+id.Name, "GIT_AUTHOR_EMAIL="+id.Email,
			"GIT_COMMITTER_NAME="+id.Name, "GIT_COMMITTER_EMAIL="+id.Email)
	}
	if id := r.auth.Committer; id.set() {
		cmd.Env = append(cmd.Env, "GIT_COMMITTER_NAME="+id.Name, "GIT_COMMITTER_EMAIL="+id.Email)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("git is not installed or not on PATH")
		}
		return out.String(), fmt.Errorf("%w\n%s", err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// configArgs configures a credential helper that answers with the token
// from the environment, replacing any helper the developer has configured,
// so the token never appears on a command line.
func (r *Repository) configArgs() []string {
	if r.auth.Token == "" {
		return nil
	}
	return []string{
		"-c", "credential.helper=",
		"-c", `credential.helper=!f() { echo username=x-access-token; echo "password=$IIDP_GIT_TOKEN"; }; f`,
	}
}
