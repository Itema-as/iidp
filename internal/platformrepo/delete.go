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
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/render"
)

// FinalBackupRetentionDays is how long a final Backup's object storage data
// is asked to be kept after iidp app delete: docs/design.md's "final backup
// kept 30 days on delete", the same number as postgres.backupRetention's
// own default (docs/implementation-notes/07-chart-postgres.md).
const FinalBackupRetentionDays = 30

// finalBackupDirPrefix names the directory a final Backup's manifests live
// in, under applications/<name>/: final-backup-<environment>, so it never
// collides with prod or staging (docs/implementation-notes/17-cli-add-capability-delete.md).
const finalBackupDirPrefix = "final-backup-"

// FinalBackup is one Environment's recorded final Backup, for the closing
// summary.
type FinalBackup struct {
	Environment string
	Cluster     string
	Namespace   string
	// Path is the directory the Backup manifest and its ArgoCD Application
	// live in, relative to the Platform repository root.
	Path        string
	RetainUntil string
}

// DeleteResult is what DeleteApplication removed and recorded.
type DeleteResult struct {
	// Deleted lists the Environment directories removed, relative to the
	// Platform repository root.
	Deleted []string
	// FinalBackups lists the final Backup recorded for each Environment
	// that had Postgres enabled, in Environment order (prod, staging).
	FinalBackups []FinalBackup
	// Files are every path committed, across both commits, in commit order.
	Files []string
}

// DeleteApplication removes application from the Platform repository in two
// commits pushed together (docs/implementation-notes/17-cli-add-capability-delete.md):
// first, for each Environment with Postgres enabled, a CloudNativePG Backup
// and the ArgoCD Application that applies it, recorded under
// applications/<name>/final-backup-<environment>/ so it survives the second
// commit; second, applications/<name>/prod/ and .../staging/ themselves,
// whose own ArgoCD Applications carry the resources finalizer and so delete
// the Environment's resources once ArgoCD notices they are gone. Both
// commits are pushed together: ArgoCD may apply the deletion before the
// Backup completes, which is why the Environment's continuous WAL archive
// and scheduled backups, already in object storage and unaffected by the
// Cluster's removal, are what actually make it recoverable, not this
// Backup's own success (logged to out, and see the implementation notes).
// A push refused because main moved is retried once from a fresh clone,
// exactly as CreateApplication does.
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

	var envs []string
	for _, e := range Environments {
		if dirExists(filepath.Join(appDir, e)) {
			envs = append(envs, e)
		}
	}

	var result DeleteResult
	var backupFiles []string
	for _, env := range envs {
		postgresEnabled, err := environmentHasPostgres(filepath.Join(appDir, env, "values.yaml"))
		if err != nil {
			return DeleteResult{}, err
		}
		if !postgresEnabled {
			continue
		}
		fb, files, err := writeFinalBackup(dir, application, env)
		if err != nil {
			return DeleteResult{}, err
		}
		result.FinalBackups = append(result.FinalBackups, fb)
		backupFiles = append(backupFiles, files...)
	}

	if len(backupFiles) > 0 {
		fmt.Fprintf(out, "Postgres is enabled; committing a final Backup for each Environment before removing it (kept %d days; the continuous WAL archive and scheduled backups already in object storage are unaffected regardless): %v\n", FinalBackupRetentionDays, backupFiles)
		if err := repo.Add(ctx, backupFiles...); err != nil {
			return DeleteResult{}, err
		}
		if err := repo.Commit(ctx, "iidp app delete "+application+": final backup"); err != nil {
			return DeleteResult{}, err
		}
		result.Files = append(result.Files, backupFiles...)
	}

	var removeDirs []string
	for _, env := range envs {
		removeDirs = append(removeDirs, path.Join(ApplicationsDir, application, env))
	}
	fmt.Fprintf(out, "Removing %v...\n", removeDirs)
	if err := repo.Remove(ctx, removeDirs...); err != nil {
		return DeleteResult{}, err
	}
	if err := repo.Commit(ctx, "iidp app delete "+application); err != nil {
		return DeleteResult{}, err
	}
	result.Deleted = removeDirs
	result.Files = append(result.Files, removeDirs...)

	if w.BeforePush != nil {
		if err := w.BeforePush(); err != nil {
			return DeleteResult{}, err
		}
	}
	fmt.Fprintln(out, "Pushing both commits together...")
	if err := repo.Push(ctx, Branch); err != nil {
		if errors.Is(err, git.ErrPushRejected) {
			return DeleteResult{}, err
		}
		return DeleteResult{}, fmt.Errorf("pushing to %s: %w\nIf this is a permission error, ask the Platform admin for write access to %s", platform.Repository, err, platform.Repository)
	}
	return result, nil
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

// writeFinalBackup writes environment's final Backup manifest and the
// ArgoCD Application that applies it under
// applications/<application>/final-backup-<environment>/, and returns the
// paths written.
func writeFinalBackup(dir, application, environment string) (FinalBackup, []string, error) {
	fullname := chartFullname(application, environment)
	cluster := render.FinalBackupCluster(fullname)
	namespace := application + "-" + environment
	retainUntil := time.Now().UTC().AddDate(0, 0, FinalBackupRetentionDays).Format("2006-01-02")

	finalBackupDir := path.Join(ApplicationsDir, application, finalBackupDirPrefix+environment)
	backupRelPath := path.Join(finalBackupDir, "backup.yaml")
	applicationRelPath := path.Join(finalBackupDir, "application.yaml")

	backupYAML, err := render.Backup(cluster, retainUntil)
	if err != nil {
		return FinalBackup{}, nil, err
	}
	applicationYAML, err := render.FinalBackupApplication(application, environment, namespace, platform.RepositoryURL, finalBackupDir)
	if err != nil {
		return FinalBackup{}, nil, err
	}

	if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(finalBackupDir)), 0o755); err != nil {
		return FinalBackup{}, nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(backupRelPath)), backupYAML, 0o644); err != nil {
		return FinalBackup{}, nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(applicationRelPath)), applicationYAML, 0o644); err != nil {
		return FinalBackup{}, nil, err
	}

	return FinalBackup{
		Environment: environment,
		Cluster:     cluster,
		Namespace:   namespace,
		Path:        finalBackupDir,
		RetainUntil: retainUntil,
	}, []string{backupRelPath, applicationRelPath}, nil
}
