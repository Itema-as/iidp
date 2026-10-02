// Package git wraps the git binary for the few operations the CLI and the
// Deploy gate need. It shells out rather than embedding a git implementation
// so the developer's own git configuration (identity, signing) applies to
// the CLI's commits.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrPushRejected is returned by Push when the remote branch has moved
// since the clone, so the push was not a fast-forward.
var ErrPushRejected = errors.New("the remote branch moved since the clone")

// Auth is the credential git presents to an HTTPS remote, and who commits
// are made as. An empty Token means no credential, as for a file:// or ssh
// remote. Identity sets author and committer; empty leaves them to the git
// configuration. Committer, when set, overrides the committer alone.
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

// CloneWithHistory is Clone with the branch's whole history, for
// FileHistory.
func CloneWithHistory(ctx context.Context, url, branch, dir string, auth Auth) (*Repository, error) {
	r := &Repository{Dir: dir, auth: auth}
	if _, err := r.run(ctx, "", "clone", "--quiet", "--single-branch", "--branch", branch, url, dir); err != nil {
		return nil, fmt.Errorf("clone %s: %w", url, err)
	}
	return r, nil
}

// Commit is one commit that changed a file.
type Commit struct {
	SHA string
	// Time is the committer date.
	Time time.Time
}

// FileHistory is the commits on HEAD that changed path, newest first, at
// most limit of them.
func (r *Repository) FileHistory(ctx context.Context, path string, limit int) ([]Commit, error) {
	out, err := r.run(ctx, r.Dir, "log", "--format=%H %cI", "-n", strconv.Itoa(limit), "HEAD", "--", path)
	if err != nil {
		return nil, fmt.Errorf("log %s: %w", path, err)
	}
	var commits []Commit
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		sha, date, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		t, err := time.Parse(time.RFC3339, date)
		if err != nil {
			return nil, fmt.Errorf("log %s: commit %s has date %q: %w", path, sha, date, err)
		}
		commits = append(commits, Commit{SHA: sha, Time: t})
	}
	return commits, nil
}

// Show is path's content at commit.
func (r *Repository) Show(ctx context.Context, commit, path string) ([]byte, error) {
	out, err := r.run(ctx, r.Dir, "show", commit+":"+path)
	if err != nil {
		return nil, fmt.Errorf("show %s:%s: %w", commit, path, err)
	}
	return []byte(out), nil
}

// Init creates a new repository on branch in the existing dir, with an
// origin remote set to url.
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

// Commit records the staged changes, as Auth's Identity when it is set and
// otherwise as the git configuration's.
func (r *Repository) Commit(ctx context.Context, message string) error {
	if _, err := r.run(ctx, r.Dir, "commit", "--quiet", "--message", message); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// CreateBranch creates and checks out a new local branch named name from
// the current HEAD.
func (r *Repository) CreateBranch(ctx context.Context, name string) error {
	if _, err := r.run(ctx, r.Dir, "checkout", "--quiet", "-b", name); err != nil {
		return fmt.Errorf("checkout -b %s: %w", name, err)
	}
	return nil
}

// Remove deletes the given paths, files or directories, and stages the
// removal, like "git rm -r".
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
