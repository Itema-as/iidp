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
// Platform's Object Storage credentials, SOPS-encrypted and without a
// namespace: the backups-credentials Secret the chart's Postgres
// Capability reads. It lives outside bootstrap/sops/, which is applied to
// the Platform's own namespaces: it is a template the CLI copies into each
// Environment that needs it.
const BackupsCredentialsFile = "bootstrap/templates/backups-credentials.enc.yaml"

// ErrBackupsCredentialsMissing is wrapped by checkBackupsCredentialsPresent
// and copyBackupsCredentials when BackupsCredentialsFile is absent from the
// Platform repository and the Postgres Capability needs it.
var ErrBackupsCredentialsMissing = errors.New("backups-credentials file is missing")

// checkBackupsCredentialsPresent refuses before anything is written when
// there is no BackupsCredentialsFile; otherwise Postgres would fail later,
// confusingly, when the chart's ObjectStore cannot find the Secret.
func checkBackupsCredentialsPresent(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(BackupsCredentialsFile))); err != nil {
		return fmt.Errorf("%w: %s has no %s; the bootstrap wizard writes it once for the Platform's age key (see bootstrap/README.md); create it before enabling Postgres", ErrBackupsCredentialsMissing, platform.Repository, BackupsCredentialsFile)
	}
	return nil
}

// copyBackupsCredentials copies BackupsCredentialsFile into the
// Environment's sops/ directory, registers it in ksops.yaml and
// kustomization.yaml, and adds the sops/ source to application.yaml once.
// It returns the repository-relative paths written or changed.
//
// The copy is byte for byte: the CLI holds no private key, so it cannot
// re-encrypt. That is safe because SOPS's MAC covers the values, not the
// file's path, and the namespace comes from the ArgoCD Application's
// destination, not the Secret.
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
	// directory listing, as writeSecrets does, so a directory that had
	// neither yet gets them.
	sopsFiles, err := registerSopsDirectory(dir, sopsDir, sopsAbs, chartFullname(application, environment))
	if err != nil {
		return nil, err
	}
	files = append(files, sopsFiles...)

	// Not render.AddSecretName: the chart references this Secret through
	// platform.backupsCredentialsSecret, not envFrom, so it must not be in
	// values.yaml's secrets: list.
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
