package platformrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/platform"
)

// FinalBackupRetentionDays is how long the Platform keeps the final Postgres
// backup the ArgoCD PreDelete hook takes when iidp app delete removes an
// Environment: docs/design.md's "final backup kept 30 days on delete", the
// same number as postgres.backupRetention's own default
// (docs/implementation-notes/07-chart-postgres.md). The CLI never writes
// this Backup or observes whether the hook succeeds (ADR-0002: it never
// talks to Kubernetes); it is used only in the confirmation prompt and the
// closing summary. See
// docs/implementation-notes/39-final-backup-predelete-hook.md.
const FinalBackupRetentionDays = 30

// DeleteResult is what DeleteApplication removed.
type DeleteResult struct {
	// Deleted lists the Environment directories DeleteApplication removed
	// application.yaml from, relative to the Platform repository root --
	// not the directories themselves, which still hold values.yaml (and
	// any secrets); see DeleteApplication's own doc comment for why.
	Deleted []string
	// PostgresEnabled is true when at least one removed Environment had
	// postgres.enabled: true, so the caller knows whether to say anything
	// about the final Backup PreDelete hook (there is nothing for it to say
	// when no Environment ever had a database).
	PostgresEnabled bool
}

// DeleteApplication removes application from the Platform repository in one
// commit: each Environment's application.yaml (applications/<name>/prod/
// and .../staging/, whichever exist) and the Application's repository
// binding (RepositoryFile), when it has one. Its own ArgoCD Application carries
// the resources finalizer, so ArgoCD deletes the Environment's resources
// once it notices application.yaml is gone -- but only after every ArgoCD
// PreDelete hook the chart renders for that Environment (the final
// Postgres Backup, when postgres.enabled, plus its ServiceAccount, Role and
// RoleBinding, chart/application/templates/final-backup-job.yaml) reaches
// Healthy. ArgoCD, not commit ordering, is what guarantees the final backup
// happens before deletion.
//
// values.yaml (and any sops/ secrets) are deliberately left in place: an
// ArgoCD PreDelete hook is only discovered by rendering the Environment's
// own Application at deletion time, and that render needs the same
// values.yaml the Application's Helm source already points at
// (application.yaml's own targetRevision tracks main, a moving branch, so
// removing values.yaml in the same commit that triggers deletion leaves
// nothing for that render to succeed against -- confirmed against a real
// kind cluster, where the resulting DeletionError condition blocks
// deletion forever; ArgoCD's own FAQ names exactly this class of problem,
// "I've deleted/corrupted my repo and can't delete my app"). Developers
// never edit the Platform repository by hand
// (docs/platform-repository.md), so this leftover directory must not need
// one either: checkApplicationAbsent in writer.go treats it as available
// (only a live application.yaml blocks a create), and attemptCreate clears
// it itself, in the same commit as the new Environment's files, if the
// same Application name is used again. See
// docs/implementation-notes/39-final-backup-predelete-hook.md for the full
// reasoning and docs/implementation-notes/17-cli-add-capability-delete.md
// for the two-commit design this replaced. A push refused because main
// moved is retried once from a fresh clone, exactly as CreateApplication
// does.
func (w *Writer) DeleteApplication(ctx context.Context, application string, out io.Writer) (DeleteResult, error) {
	return runWithRetry(ctx, func(ctx context.Context, retry bool) (DeleteResult, error) {
		return w.attemptDelete(ctx, application, out, retry)
	})
}

func (w *Writer) attemptDelete(ctx context.Context, application string, out io.Writer, _ bool) (DeleteResult, error) {
	dir, err := os.MkdirTemp("", "iidp-platform-")
	if err != nil {
		return DeleteResult{}, err
	}
	defer os.RemoveAll(dir)

	repo, err := git.Clone(ctx, w.URL, Branch, dir, w.Auth)
	if err != nil {
		return DeleteResult{}, fmt.Errorf("cloning %s: %w", platform.Repository, err)
	}

	appDir := filepath.Join(dir, filepath.FromSlash(ApplicationsDir), application)
	if _, err := os.Stat(appDir); errors.Is(err, fs.ErrNotExist) {
		return DeleteResult{}, fmt.Errorf("%w: %q has no directory under %s/ in %s", ErrApplicationMissing, application, ApplicationsDir, platform.Repository)
	} else if err != nil {
		return DeleteResult{}, fmt.Errorf("checking for the Application: %w", err)
	}

	var removeDirs []string
	var removeFiles []string
	var postgresEnabled bool
	for _, e := range Environments {
		envDir := filepath.Join(appDir, e)
		applicationYAML := filepath.Join(envDir, "application.yaml")
		if _, err := os.Stat(applicationYAML); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return DeleteResult{}, fmt.Errorf("checking for %s/%s/application.yaml: %w", application, e, err)
		}
		removeDirs = append(removeDirs, path.Join(ApplicationsDir, application, e))
		removeFiles = append(removeFiles, path.Join(ApplicationsDir, application, e, "application.yaml"))
		enabled, err := environmentHasPostgres(filepath.Join(envDir, "values.yaml"))
		if err != nil {
			return DeleteResult{}, err
		}
		postgresEnabled = postgresEnabled || enabled
	}

	if len(removeFiles) == 0 {
		// appDir exists (checked above) but neither Environment has a live
		// application.yaml: either a previous iidp app delete already ran
		// (its own leftover values.yaml is still there, on purpose -- see
		// DeleteApplication's doc comment) or a hand edit removed it. Refuse
		// with the same clear message as "never existed", rather than
		// proceeding to a git rm with nothing to remove.
		return DeleteResult{}, fmt.Errorf("%w: %q has no Environment with a live application.yaml under %s/ in %s", ErrApplicationMissing, application, ApplicationsDir, platform.Repository)
	}

	// The repository binding goes in the same commit: nothing renders
	// from it, so ArgoCD's PreDelete hook does not need it, and a deleted
	// Application must not stay deployable through the Deploy gate by its
	// old repository (docs/implementation-notes/58-repository-binding.md).
	switch _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(RepositoryBindingPath(application)))); {
	case err == nil:
		removeFiles = append(removeFiles, RepositoryBindingPath(application))
	case !errors.Is(err, fs.ErrNotExist):
		return DeleteResult{}, fmt.Errorf("checking for %s: %w", RepositoryBindingPath(application), err)
	}

	fmt.Fprintf(out, "Removing %v...\n", removeFiles)
	if err := repo.Remove(ctx, removeFiles...); err != nil {
		return DeleteResult{}, err
	}
	if err := repo.Commit(ctx, "iidp app delete "+application); err != nil {
		return DeleteResult{}, err
	}

	if w.BeforePush != nil {
		if err := w.BeforePush(); err != nil {
			return DeleteResult{}, err
		}
	}
	fmt.Fprintln(out, "Pushing...")
	if err := repo.Push(ctx, Branch); err != nil {
		if errors.Is(err, git.ErrPushRejected) {
			return DeleteResult{}, err
		}
		return DeleteResult{}, fmt.Errorf("pushing to %s: %w\nIf this is a permission error, ask the Platform admin for write access to %s", platform.Repository, err, platform.Repository)
	}
	return DeleteResult{Deleted: removeDirs, PostgresEnabled: postgresEnabled}, nil
}

// environmentHasPostgres reads postgres.enabled out of an Environment's
// values.yaml, so DeleteApplication knows whether to mention the final
// Backup PreDelete hook -- it never writes anything Postgres-specific
// itself (docs/implementation-notes/39-final-backup-predelete-hook.md).
func environmentHasPostgres(valuesPath string) (bool, error) {
	data, err := os.ReadFile(valuesPath)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", valuesPath, err)
	}
	var v environmentState
	if err := yaml.Unmarshal(data, &v); err != nil {
		return false, fmt.Errorf("parsing %s: %w", valuesPath, err)
	}
	return v.Postgres.Enabled, nil
}
