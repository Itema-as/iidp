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

// EnvironmentAuto is the Environment a deploy asks for when it leaves the
// choice to the Platform: the Deploy gate turns it into staging or prod
// from the Platform repository and the ref being deployed, never from what
// the workflow says (docs/implementation-notes/60-deploy-gate.md).
const EnvironmentAuto = "auto"

// ImageTagChange is one Deploy or Promote: a new image tag for one
// Environment of an Application.
type ImageTagChange struct {
	Application string
	Tag         string
	// Environment decides, on each fresh clone at dir, which Environment
	// the tag is written to, or refuses the change by returning an error.
	// It runs once the Application is known to have a directory, so it can
	// read the Application's binding and look for a staging Environment in
	// the same clone the write is made from. Nothing is written when it
	// errors.
	Environment func(dir string) (string, error)
	// Body, when set, follows the commit subject after a blank line.
	Body string
	// MigrationCommand, when not nil, is written into
	// postgres.migrationCommand in the same commit as the tag: "" clears
	// it. nil leaves it as it is. A non-empty command for an Environment
	// without Postgres is refused with ErrPostgresMissing, and nothing is
	// written (docs/implementation-notes/66-migration-command-in-repo.md).
	MigrationCommand *string
}

// ErrPostgresMissing is wrapped when a deploy carries a migration command
// for an Environment that has no Postgres Capability.
var ErrPostgresMissing = errors.New("the Environment has no Postgres Capability")

// DeployResult is what SetImageTag wrote.
type DeployResult struct {
	// Environment is the Environment the tag was written to.
	Environment string
	// File is the values file, relative to the Platform repository root.
	File string
	// Commit is the commit that wrote it, or "" when Unchanged.
	Commit string
	// Unchanged is true when the Environment already ran this tag with
	// this migration command: nothing was committed, so a retried request
	// is harmless.
	Unchanged bool
	// MigrationCommandChanged is true when the commit changed
	// postgres.migrationCommand.
	MigrationCommandChanged bool
}

// SetImageTag writes change.Tag into image.tag of an Environment's
// values.yaml. It clones main, refuses an Application with no directory,
// asks change.Environment which Environment to write, refuses one without
// a live application.yaml, edits image.tag in place (every other key, and
// every comment, untouched), and postgres.migrationCommand too when the
// change carries one, commits both as "Deploy <application> <environment>
// <tag>" and pushes, with the same retry-once-on-a-moved-main logic as
// CreateApplication. Every check runs again on the retry's fresh clone.
func (w *Writer) SetImageTag(ctx context.Context, change ImageTagChange) (DeployResult, error) {
	if change.Environment == nil {
		return DeployResult{}, errors.New("SetImageTag: no Environment decision")
	}
	return runWithRetry(ctx, func(ctx context.Context, _ bool) (DeployResult, error) {
		return w.attemptSetImageTag(ctx, change)
	})
}

func (w *Writer) attemptSetImageTag(ctx context.Context, change ImageTagChange) (DeployResult, error) {
	dir, err := os.MkdirTemp("", "iidp-platform-")
	if err != nil {
		return DeployResult{}, err
	}
	defer os.RemoveAll(dir)

	repo, err := git.Clone(ctx, w.URL, Branch, dir, w.Auth)
	if err != nil {
		return DeployResult{}, fmt.Errorf("cloning %s: %w", platform.Repository, err)
	}

	appDir := filepath.Join(dir, filepath.FromSlash(ApplicationsDir), change.Application)
	switch _, err := os.Stat(appDir); {
	case errors.Is(err, fs.ErrNotExist):
		return DeployResult{}, fmt.Errorf("%w: %q has no directory under %s/ in %s", ErrApplicationMissing, change.Application, ApplicationsDir, platform.Repository)
	case err != nil:
		return DeployResult{}, fmt.Errorf("checking for the Application: %w", err)
	}

	environment, err := change.Environment(dir)
	if err != nil {
		return DeployResult{}, err
	}
	live, err := HasEnvironment(dir, change.Application, environment)
	if err != nil {
		return DeployResult{}, err
	}
	envDir := EnvironmentDir(change.Application, environment)
	if !live {
		return DeployResult{}, fmt.Errorf("%w: %s has no %s Environment for %q (expected %s/application.yaml)", ErrEnvironmentMissing, platform.Repository, environment, change.Application, envDir)
	}

	valuesRelPath := path.Join(envDir, "values.yaml")
	valuesAbs := filepath.Join(dir, filepath.FromSlash(valuesRelPath))
	data, err := os.ReadFile(valuesAbs)
	if err != nil {
		return DeployResult{}, fmt.Errorf("reading %s: %w", valuesRelPath, err)
	}
	data, changed, err := render.SetImageTag(data, change.Tag)
	if err != nil {
		return DeployResult{}, fmt.Errorf("%s: %w", valuesRelPath, err)
	}
	res := DeployResult{Environment: environment, File: valuesRelPath}
	var migrationNote string
	if change.MigrationCommand != nil {
		command := *change.MigrationCommand
		data, res.MigrationCommandChanged, err = render.SetMigrationCommand(data, command)
		switch {
		case errors.Is(err, render.ErrNoPostgres):
			return DeployResult{}, fmt.Errorf("%w: %s's %s Environment has no database to migrate, so it cannot take the migration command in iidp.yaml. Add the Postgres Capability first (iidp app add-capability %s --postgres), or remove migrationCommand from iidp.yaml", ErrPostgresMissing, change.Application, environment, change.Application)
		case err != nil:
			return DeployResult{}, fmt.Errorf("%s: %w", valuesRelPath, err)
		case res.MigrationCommandChanged && command == "":
			migrationNote = "Clear the migration command: iidp.yaml sets none."
		case res.MigrationCommandChanged:
			migrationNote = "Migration command, from iidp.yaml: " + command
		}
		changed = changed || res.MigrationCommandChanged
	}
	if !changed {
		res.Unchanged = true
		return res, nil
	}
	if err := os.WriteFile(valuesAbs, data, 0o644); err != nil {
		return DeployResult{}, err
	}

	if err := repo.Add(ctx, valuesRelPath); err != nil {
		return DeployResult{}, err
	}
	message := fmt.Sprintf("Deploy %s %s %s", change.Application, environment, change.Tag)
	if migrationNote != "" {
		message += "\n\n" + migrationNote
	}
	if change.Body != "" {
		message += "\n\n" + change.Body
	}
	if err := repo.Commit(ctx, message); err != nil {
		return DeployResult{}, err
	}
	if w.BeforePush != nil {
		if err := w.BeforePush(); err != nil {
			return DeployResult{}, err
		}
	}
	if err := repo.Push(ctx, Branch); err != nil {
		if errors.Is(err, git.ErrPushRejected) {
			return DeployResult{}, err
		}
		return DeployResult{}, fmt.Errorf("pushing to %s: %w\nIf this is a permission error, the GitHub App needs contents: write on %s", platform.Repository, err, platform.Repository)
	}
	res.Commit, err = repo.Head(ctx)
	if err != nil {
		return DeployResult{}, err
	}
	return res, nil
}

// HasEnvironment reports whether application has a live environment (an
// application.yaml ArgoCD applies) in the clone of the Platform repository
// at dir.
func HasEnvironment(dir, application, environment string) (bool, error) {
	if err := ValidateEnvironmentName(environment); err != nil {
		return false, err
	}
	switch _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(EnvironmentDir(application, environment)), "application.yaml")); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("checking for the %s Environment: %w", environment, err)
	}
}
