package platformrepo

import (
	"context"
	"crypto/rand"
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

// ErrNoDatabase is wrapped by SetDatabaseAccess for an Environment without
// the Postgres Capability.
var ErrNoDatabase = errors.New("the Environment has no database")

// passwordSlug is the key-slug part of a database access role's password
// Secret name and file name, as SecretSlug is for a secret's KEY. Each is
// a slug iidp secret set refuses (ValidateSecretKey), so neither can be
// overwritten by a secret of the same name.
func passwordSlug(role render.AccessRole) string {
	if role == render.ReadOnlyRole {
		return "db-read"
	}
	return "db-write"
}

// PasswordSecretName is the Secret holding role's password in an
// Environment: <fullname>-db-write or <fullname>-db-read.
func PasswordSecretName(application, environment string, role render.AccessRole) string {
	return chartFullname(application, environment) + "-" + passwordSlug(role)
}

// DatabaseAccessResult is what SetDatabaseAccess wrote.
type DatabaseAccessResult struct {
	Config Config
	// Access is the Environment's levels once the run is done.
	Access render.DatabaseAccess
	// Files are the paths written or removed, relative to the Platform
	// repository root; none when nothing changed and nothing was committed.
	Files []string
}

// SetDatabaseAccess sets an Environment's database access levels to change,
// where "" keeps a level as it is, and writes the passwords those levels
// need, in one commit. A push refused because main moved is retried once
// from a fresh clone.
func (w *Writer) SetDatabaseAccess(ctx context.Context, application, environment string, change render.DatabaseAccess) (DatabaseAccessResult, error) {
	return runWithRetry(ctx, func(ctx context.Context, _ bool) (DatabaseAccessResult, error) {
		return w.attemptSetDatabaseAccess(ctx, application, environment, change)
	})
}

func (w *Writer) attemptSetDatabaseAccess(ctx context.Context, application, environment string, change render.DatabaseAccess) (DatabaseAccessResult, error) {
	dir, err := os.MkdirTemp("", "iidp-platform-")
	if err != nil {
		return DatabaseAccessResult{}, err
	}
	defer os.RemoveAll(dir)

	repo, err := git.Clone(ctx, w.URL, Branch, dir, w.Auth)
	if err != nil {
		return DatabaseAccessResult{}, fmt.Errorf("cloning %s: %w", platform.Repository, err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		return DatabaseAccessResult{}, err
	}
	live, err := HasEnvironment(dir, application, environment)
	if err != nil {
		return DatabaseAccessResult{}, err
	}
	if !live {
		return DatabaseAccessResult{}, fmt.Errorf("%w: %s has no %s Environment for %q; create it with iidp app create first", ErrEnvironmentMissing, platform.Repository, environment, application)
	}

	valuesRelPath := path.Join(EnvironmentDir(application, environment), "values.yaml")
	values, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(valuesRelPath)))
	if err != nil {
		return DatabaseAccessResult{}, fmt.Errorf("reading %s: %w", valuesRelPath, err)
	}
	current, err := render.ReadDatabase(values)
	if err != nil {
		return DatabaseAccessResult{}, fmt.Errorf("%s: %w", valuesRelPath, err)
	}
	if !current.Enabled {
		// The levels given are checked first, so a level other than none
		// is refused with the chart's own message.
		if _, err := render.ResolveDatabaseAccess(environment, false, change); err != nil {
			return DatabaseAccessResult{}, err
		}
		return DatabaseAccessResult{}, fmt.Errorf("%w: the %s Environment of %q has no Postgres Capability; add it with iidp app add-capability %s --postgres", ErrNoDatabase, environment, application, application)
	}
	access := current.Access
	if change.ReadWrite != "" {
		access.ReadWrite = change.ReadWrite
	}
	if change.ReadOnly != "" {
		access.ReadOnly = change.ReadOnly
	}
	if access, err = render.ResolveDatabaseAccess(environment, true, access); err != nil {
		return DatabaseAccessResult{}, err
	}

	files, err := w.writeDatabaseAccess(ctx, dir, cfg, application, environment, access, true)
	if err != nil {
		return DatabaseAccessResult{}, err
	}
	res := DatabaseAccessResult{Config: cfg, Access: access}
	if len(files) == 0 {
		return res, nil
	}
	if err := repo.Add(ctx, files...); err != nil {
		return DatabaseAccessResult{}, err
	}
	message := fmt.Sprintf("iidp app db access %s --env %s --read-write %s --read-only %s", application, environment, access.ReadWrite, access.ReadOnly)
	if err := repo.Commit(ctx, message); err != nil {
		return DatabaseAccessResult{}, err
	}
	if w.BeforePush != nil {
		if err := w.BeforePush(); err != nil {
			return DatabaseAccessResult{}, err
		}
	}
	if err := repo.Push(ctx, Branch); err != nil {
		if errors.Is(err, git.ErrPushRejected) {
			return DatabaseAccessResult{}, err
		}
		return DatabaseAccessResult{}, fmt.Errorf("pushing to %s: %w\nIf this is a permission error, ask the Platform admin for write access to %s", platform.Repository, err, platform.Repository)
	}
	res.Files = files
	return res, nil
}

// writeDatabaseAccess writes access, already checked, into an Environment
// with Postgres in the clone at dir, and returns the repository-relative
// paths written or removed, none when nothing changed. A level other than
// none gets its role's password, generated and SOPS-encrypted unless it is
// already there; a level of none loses it. With passwords false only the
// levels are written: the Environment stays closed until a later run
// writes the passwords.
func (w *Writer) writeDatabaseAccess(ctx context.Context, dir string, cfg Config, application, environment string, access render.DatabaseAccess, passwords bool) ([]string, error) {
	envDir := EnvironmentDir(application, environment)
	sopsDir := path.Join(envDir, "sops")
	sopsAbs := filepath.Join(dir, filepath.FromSlash(sopsDir))
	valuesRelPath := path.Join(envDir, "values.yaml")
	valuesAbs := filepath.Join(dir, filepath.FromSlash(valuesRelPath))
	values, err := os.ReadFile(valuesAbs)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", valuesRelPath, err)
	}
	values, valuesChanged, err := render.SetDatabaseAccess(values, access)
	if err != nil {
		return nil, err
	}
	current, err := render.ReadDatabase(values)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", valuesRelPath, err)
	}

	var files []string
	for _, role := range render.AccessRoles {
		fileRelPath := path.Join(sopsDir, passwordSlug(role)+".enc.yaml")
		fileAbs := filepath.Join(dir, filepath.FromSlash(fileRelPath))
		_, statErr := os.Stat(fileAbs)
		present := statErr == nil
		if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			return nil, statErr
		}
		name := ""
		switch {
		case role.Level(access) == render.AccessNone:
			if present {
				if err := os.Remove(fileAbs); err != nil {
					return nil, err
				}
				files = append(files, fileRelPath)
			}
		case !passwords:
			name = current.PasswordSecret(role)
		case present && current.PasswordSecret(role) != "":
			name = current.PasswordSecret(role)
		default:
			name = PasswordSecretName(application, environment, role)
			if err := w.writePassword(ctx, cfg, fileRelPath, fileAbs, render.PasswordSecret{
				Name:        name,
				Application: application,
				Environment: environment,
				Username:    role.PostgresRole(application),
				Password:    rand.Text(),
			}); err != nil {
				return nil, err
			}
			files = append(files, fileRelPath)
		}
		var changed bool
		if values, changed, err = render.SetPasswordSecret(values, role, name); err != nil {
			return nil, err
		}
		valuesChanged = valuesChanged || changed
	}

	if len(files) > 0 {
		sopsFiles, err := registerSopsDirectory(dir, sopsDir, sopsAbs, chartFullname(application, environment))
		if err != nil {
			return nil, err
		}
		files = append(files, sopsFiles...)
		appRelPath := path.Join(envDir, "application.yaml")
		appAbs := filepath.Join(dir, filepath.FromSlash(appRelPath))
		appData, err := os.ReadFile(appAbs)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", appRelPath, err)
		}
		appData, appChanged, err := render.AddKustomizeSource(appData, platform.RepositoryURL, sopsDir)
		if err != nil {
			return nil, err
		}
		if appChanged {
			if err := os.WriteFile(appAbs, appData, 0o644); err != nil {
				return nil, err
			}
			files = append(files, appRelPath)
		}
	}
	if valuesChanged {
		if err := os.WriteFile(valuesAbs, values, 0o644); err != nil {
			return nil, err
		}
		files = append(files, valuesRelPath)
	}
	if len(files) == 0 {
		return nil, nil
	}
	previewFiles, err := refreshPreviews(dir, application, cfg, files)
	if err != nil {
		return nil, err
	}
	return append(files, previewFiles...), nil
}

// writePassword encrypts a password Secret with the Platform's age key and
// writes it into the clone.
func (w *Writer) writePassword(ctx context.Context, cfg Config, relPath, absPath string, secret render.PasswordSecret) error {
	if cfg.AgePublicKey == "" {
		return fmt.Errorf("%w: %s in %s sets no agePublicKey, so the database password cannot be encrypted", ErrAgePublicKeyMissing, ConfigFile, platform.Repository)
	}
	plaintext, err := render.PasswordSecretDocument(secret)
	if err != nil {
		return err
	}
	encrypted, err := w.encryptor().Encrypt(ctx, plaintext, cfg.AgePublicKey, relPath)
	if err != nil {
		return fmt.Errorf("encrypting %s's password: %w", secret.Username, err)
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(absPath, encrypted, 0o644)
}
