package platformrepo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/render"
)

// EnvironmentAuto is the environment argument iidp ci set-image accepts
// besides prod and staging: it targets staging when the Application has
// one and prod otherwise, decided from the Platform repository, never by
// the caller (docs/implementation-notes/12-deploy-workflow.md).
const EnvironmentAuto = "auto"

// SetImageTag writes tag into image.tag of application's environment
// values.yaml, where environment is "prod", "staging" or EnvironmentAuto.
// It clones main, resolves auto (if given) by checking whether the
// Application has a staging Environment, edits image.tag in place (every
// other key, and every comment, untouched), commits
// "Deploy <application> <environment> <tag>" and pushes, with the same
// push-and-retry-on-moved-main logic as CreateApplication.
func (w *Writer) SetImageTag(ctx context.Context, application, environment, tag string) (Result, error) {
	return runWithRetry(ctx, func(ctx context.Context, _ bool) (Result, error) {
		return w.attemptSetImageTag(ctx, application, environment, tag)
	})
}

func (w *Writer) attemptSetImageTag(ctx context.Context, application, environment, tag string) (Result, error) {
	dir, err := os.MkdirTemp("", "iidp-platform-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)

	repo, err := git.Clone(ctx, w.URL, Branch, dir, w.Auth)
	if err != nil {
		return Result{}, fmt.Errorf("cloning %s: %w", platform.Repository, err)
	}
	// Purely informational: platform.yaml's githubApp.installationId, when
	// the wizard recorded one, documents which installation this command
	// is expected to run as. Nothing here depends on it (see
	// docs/implementation-notes/12-deploy-workflow.md).
	documentedInstallationID, hasDocumentedInstallationID := PeekGitHubAppInstallationID(dir)

	// ErrApplicationMissing is the same error internal/platformrepo/capability.go
	// wraps for add-capability's "unknown Application" refusal.
	appDir := filepath.Join(dir, filepath.FromSlash(ApplicationsDir), application)
	switch _, err := os.Stat(appDir); {
	case errors.Is(err, fs.ErrNotExist):
		return Result{}, fmt.Errorf("%w: %q has no directory under %s/ in %s", ErrApplicationMissing, application, ApplicationsDir, platform.Repository)
	case err != nil:
		return Result{}, fmt.Errorf("checking for the Application: %w", err)
	}

	resolved, err := resolveEnvironment(appDir, environment)
	if err != nil {
		return Result{}, err
	}

	envDir := EnvironmentDir(application, resolved)
	switch _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(envDir))); {
	case errors.Is(err, fs.ErrNotExist):
		return Result{}, fmt.Errorf("%w: %s has no %s Environment for %q (expected %s/)", ErrEnvironmentMissing, platform.Repository, resolved, application, envDir)
	case err != nil:
		return Result{}, fmt.Errorf("checking for the Environment: %w", err)
	}

	valuesRelPath := path.Join(envDir, "values.yaml")
	valuesAbs := filepath.Join(dir, filepath.FromSlash(valuesRelPath))
	data, err := os.ReadFile(valuesAbs)
	if err != nil {
		return Result{}, fmt.Errorf("reading %s: %w", valuesRelPath, err)
	}
	data, _, err = render.SetImageTag(data, tag)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", valuesRelPath, err)
	}
	if err := os.WriteFile(valuesAbs, data, 0o644); err != nil {
		return Result{}, err
	}

	if err := repo.Add(ctx, valuesRelPath); err != nil {
		return Result{}, err
	}
	message := fmt.Sprintf("Deploy %s %s %s", application, resolved, tag)
	if err := repo.Commit(ctx, message); err != nil {
		return Result{}, err
	}
	if w.BeforePush != nil {
		if err := w.BeforePush(); err != nil {
			return Result{}, err
		}
	}
	if err := repo.Push(ctx, Branch); err != nil {
		if errors.Is(err, git.ErrPushRejected) {
			return Result{}, err
		}
		return Result{}, fmt.Errorf("pushing to %s: %w\nIf this is a permission error, the GitHub App needs contents: write on %s", platform.Repository, err, platform.Repository)
	}
	res := Result{Files: []string{valuesRelPath}, Environment: resolved}
	if hasDocumentedInstallationID {
		res.DocumentedGitHubAppInstallationID = documentedInstallationID
	}
	return res, nil
}

// resolveEnvironment turns environment (prod, staging or EnvironmentAuto)
// into the Environment SetImageTag actually writes: EnvironmentAuto
// resolves to staging when appDir (the Application's directory in the
// already-cloned Platform repository) has one, prod otherwise. prod and
// staging pass through validated as themselves.
func resolveEnvironment(appDir, environment string) (string, error) {
	if environment != EnvironmentAuto {
		if err := ValidateEnvironmentName(environment); err != nil {
			return "", err
		}
		return environment, nil
	}
	switch _, err := os.Stat(filepath.Join(appDir, "staging")); {
	case err == nil:
		return "staging", nil
	case errors.Is(err, fs.ErrNotExist):
		return "prod", nil
	default:
		return "", fmt.Errorf("checking for a staging Environment: %w", err)
	}
}
