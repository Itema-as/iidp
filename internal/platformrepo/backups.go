package platformrepo

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/render"
)

// BackupsCredentialsFile is where the bootstrap wizard writes the
// Platform's Object Storage credentials once, SOPS-encrypted with the
// Platform's age key and carrying no namespace: a Kubernetes Secret named
// backups-credentials with stringData ACCESS_KEY_ID and ACCESS_SECRET_KEY,
// the keys chart/application's Postgres Capability reads through
// platform.backupsCredentialsSecret
// (docs/implementation-notes/07-chart-postgres.md). It deliberately lives
// outside bootstrap/sops/, which the platform-secrets Application applies
// into every namespace of the Platform's own components: this file is a
// template the CLI copies, applied only where an Environment copies it
// (docs/implementation-notes/42-backups-credentials.md).
const BackupsCredentialsFile = "bootstrap/templates/backups-credentials.enc.yaml"

// ErrBackupsCredentialsMissing is wrapped by checkBackupsCredentialsPresent
// and copyBackupsCredentials when BackupsCredentialsFile is absent from the
// Platform repository and the Postgres Capability needs it.
var ErrBackupsCredentialsMissing = errors.New("backups-credentials file is missing")

// checkBackupsCredentialsPresent refuses clearly, before anything else is
// written, when the Platform repository has no BackupsCredentialsFile: the
// bootstrap wizard writes it once, and enabling Postgres without it would
// otherwise fail confusingly, later, when the chart's ObjectStore cannot
// find a Secret.
func checkBackupsCredentialsPresent(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(BackupsCredentialsFile))); err != nil {
		return fmt.Errorf("%w: %s has no %s; the bootstrap wizard writes it once for the Platform's age key (see bootstrap/README.md); create it before enabling Postgres", ErrBackupsCredentialsMissing, platform.Repository, BackupsCredentialsFile)
	}
	return nil
}

// copyBackupsCredentials copies BackupsCredentialsFile, byte for byte, into
// application's environment's sops/ directory as
// backups-credentials.enc.yaml, then registers it in that directory's
// ksops.yaml/kustomization.yaml (created exactly as iidp secret set creates
// them, if they do not exist yet) and adds the sops/ kustomize source to
// the Environment's application.yaml, exactly once
// (render.AddKustomizeSource matches by path and is a no-op if a secret has
// already added it). It returns the repository-relative paths written or
// changed.
//
// The copy is byte for byte, never decrypted: the CLI holds no private key
// (docs/platform-repository.md, "Secrets"), so it could not re-encrypt for
// a new path even if it wanted to. This is safe because SOPS does not bind
// a document to the path it was encrypted for -- its MAC covers the
// (decrypted) values, not the file name or location, confirmed by
// decrypting a copy of the file from a different path in
// internal/cli/app_postgres_backups_credentials_test.go and in
// test/wizard/run.sh -- and because the destination namespace comes from
// the Environment's own ArgoCD Application destination
// (spec.destination.namespace), never from anything inside the Secret
// document itself, the same reasoning already applies to every
// SOPS-encrypted Secret this repository writes (docs/platform-repository.md,
// "No namespace: the ArgoCD Application's spec.destination.namespace
// applies").
func copyBackupsCredentials(dir, application, environment string) ([]string, error) {
	srcPath := filepath.Join(dir, filepath.FromSlash(BackupsCredentialsFile))
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %s has no %s; the bootstrap wizard writes it once for the Platform's age key (see bootstrap/README.md); create it before enabling Postgres", ErrBackupsCredentialsMissing, platform.Repository, BackupsCredentialsFile)
	}

	envDir := EnvironmentDir(application, environment)
	sopsDir := path.Join(envDir, "sops")
	sopsAbs := filepath.Join(dir, filepath.FromSlash(sopsDir))
	if err := os.MkdirAll(sopsAbs, 0o755); err != nil {
		return nil, err
	}

	destRelPath := path.Join(sopsDir, "backups-credentials.enc.yaml")
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(destRelPath)), data, 0o644); err != nil {
		return nil, err
	}
	files := []string{destRelPath}

	// kustomization.yaml and ksops.yaml are rewritten from a fresh
	// directory listing, the same bookkeeping iidp secret set's
	// writeSecrets does after adding or changing a file here: it means a
	// directory that had neither yet (Postgres enabled before any
	// iidp secret set) gets them created here.
	sopsFiles, err := registerSopsDirectory(dir, sopsDir, sopsAbs, chartFullname(application, environment))
	if err != nil {
		return nil, err
	}
	files = append(files, sopsFiles...)

	// Deliberately not render.AddSecretName: the chart references this
	// Secret through platform.backupsCredentialsSecret
	// (chart/application/values.yaml), not through envFrom/secrets:, so it
	// must not be added to values.yaml's secrets: list
	// (docs/implementation-notes/42-backups-credentials.md).
	appRelPath := path.Join(envDir, "application.yaml")
	appAbsPath := filepath.Join(dir, filepath.FromSlash(appRelPath))
	appData, err := os.ReadFile(appAbsPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", appRelPath, err)
	}
	appData, appChanged, err := render.AddKustomizeSource(appData, platform.RepositoryURL, sopsDir)
	if err != nil {
		return nil, err
	}
	if appChanged {
		if err := os.WriteFile(appAbsPath, appData, 0o644); err != nil {
			return nil, err
		}
		files = append(files, appRelPath)
	}

	return files, nil
}
