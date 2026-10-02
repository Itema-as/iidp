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
// backup the chart's PreDelete hook takes, the same as
// postgres.backupRetention's default. The CLI only prints it: it never
// talks to Kubernetes.
const FinalBackupRetentionDays = 30

// DeleteResult is what DeleteApplication removed.
type DeleteResult struct {
	// Deleted lists the Environment directories DeleteApplication removed
	// application.yaml from, relative to the Platform repository root. The
	// directories themselves stay.
	Deleted []string
	// PostgresEnabled is true when at least one removed Environment had
	// postgres.enabled: true, so the caller knows whether to mention the
	// final backup.
	PostgresEnabled bool
	// Previews is true when the Application had Preview Environments,
	// whose ApplicationSet was removed with the rest.
	Previews bool
}

// DeleteApplication removes application from the Platform repository in one
// commit: each Environment's application.yaml, the repository binding and
// the previews ApplicationSet. ArgoCD then deletes each Environment's
// resources, but only after the chart's PreDelete hooks (the final
// Postgres backup) are Healthy.
//
// values.yaml and any sops/ secrets are left in place: ArgoCD finds the
// PreDelete hook by rendering the Environment at deletion time from main,
// and without values.yaml that render fails with a DeletionError that
// blocks deletion forever. attemptCreate clears the leftover directory if
// the name is used again. A push refused because main moved is retried
// once from a fresh clone.
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
		// Only a leftover directory, from an earlier delete or a hand edit:
		// refuse as for an Application that never existed, rather than run
		// git rm with nothing to remove.
		return DeleteResult{}, fmt.Errorf("%w: %q has no Environment with a live application.yaml under %s/ in %s", ErrApplicationMissing, application, ApplicationsDir, platform.Repository)
	}

	// The repository binding goes in the same commit: unlike values.yaml,
	// nothing renders from it, and a deleted Application must not stay
	// deployable through the Deploy gate by its old repository.
	switch _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(RepositoryBindingPath(application)))); {
	case err == nil:
		removeFiles = append(removeFiles, RepositoryBindingPath(application))
	case !errors.Is(err, fs.ErrNotExist):
		return DeleteResult{}, fmt.Errorf("checking for %s: %w", RepositoryBindingPath(application), err)
	}
	// So do the Preview Environments: ArgoCD deletes the ApplicationSet,
	// which deletes every preview with its namespace. They take no final
	// backup, and they render from staging's values.yaml and sops/, which
	// stay, like every Environment's.
	previews, err := previewsPresent(dir, application)
	if err != nil {
		return DeleteResult{}, err
	}
	if previews {
		removeFiles = append(removeFiles, PreviewsPath(application))
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
	return DeleteResult{Deleted: removeDirs, PostgresEnabled: postgresEnabled, Previews: previews}, nil
}

// environmentHasPostgres reads postgres.enabled out of an Environment's
// values.yaml.
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
