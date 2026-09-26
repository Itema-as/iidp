// Package release_test runs the release workflow's major-tag step
// (.github/workflows/release.yaml, job major-tag) against throwaway git
// repositories, the way GitHub Actions would: in a checkout of the release
// tag, with origin being the repository to push to.
package release_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func majorTagScript(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../.github/workflows/release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	for _, step := range wf.Jobs["major-tag"].Steps {
		if step.Run != "" {
			return step.Run
		}
	}
	t.Fatal("release.yaml has no major-tag job with a run step")
	return ""
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// origin has three commits tagged v0.1.0, v0.2.0 and v0.3.0-rc1 on a line,
// plus v0 wherever start says. The step runs in a clone checked out at
// release, and the test returns where v0 points on origin afterwards.
func runMajorTag(t *testing.T, release, start string) (v0 string, commits map[string]string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	work := filepath.Join(root, "work")
	git(t, root, "init", "-q", "--bare", "-b", "main", origin)
	git(t, root, "init", "-q", "-b", "main", work)
	commits = map[string]string{}
	for _, tag := range []string{"v0.1.0", "v0.2.0", "v0.3.0-rc1"} {
		git(t, work, "commit", "-q", "--allow-empty", "-m", tag)
		git(t, work, "tag", "-a", tag, "-m", tag)
		commits[tag] = git(t, work, "rev-parse", "HEAD")
	}
	if start != "" {
		git(t, work, "tag", "v0", commits[start])
	}
	git(t, work, "push", "-q", origin, "main", "--tags")

	checkout := filepath.Join(root, "checkout")
	git(t, root, "clone", "-q", origin, checkout)
	git(t, checkout, "checkout", "-q", release)
	cmd := exec.Command("bash", "-c", majorTagScript(t))
	cmd.Dir = checkout
	cmd.Env = append(os.Environ(), "GITHUB_REF_NAME="+release, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "TMPDIR="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("major-tag step for %s: %v\n%s", release, err, out)
	}
	cmd = exec.Command("git", "rev-parse", "-q", "--verify", "refs/tags/v0^{commit}")
	cmd.Dir = origin
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out)), commits
}

func TestMajorTagMovesForwardOnly(t *testing.T) {
	for _, tc := range []struct {
		name, release, start, want string
	}{
		{"first release creates v0", "v0.1.0", "", "v0.1.0"},
		{"a newer release moves v0 forward", "v0.2.0", "v0.1.0", "v0.2.0"},
		{"an older release finishing late leaves v0", "v0.1.0", "v0.2.0", "v0.2.0"},
		{"a pre-release leaves v0", "v0.3.0-rc1", "v0.2.0", "v0.2.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v0, commits := runMajorTag(t, tc.release, tc.start)
			if v0 != commits[tc.want] {
				t.Errorf("v0 on origin = %q, want %s (%s)", v0, tc.want, commits[tc.want])
			}
		})
	}
}
