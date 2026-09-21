package platformrepo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/render"
)

// Environments is the two Environments an Application may have. Both
// directory names and the layout in docs/platform-repository.md are fixed
// to these two.
var Environments = []string{"prod", "staging"}

// ErrInvalidEnvironment is wrapped by ValidateEnvironmentName.
var ErrInvalidEnvironment = errors.New("invalid Environment")

// ErrInvalidSecretKey is wrapped by ValidateSecretKey.
var ErrInvalidSecretKey = errors.New("invalid secret KEY")

// ErrEnvironmentMissing is wrapped by SetSecrets when the Application has no
// such Environment yet.
var ErrEnvironmentMissing = errors.New("Environment does not exist")

// ErrAgePublicKeyMissing is wrapped by SetSecrets when platform.yaml sets no
// agePublicKey.
var ErrAgePublicKeyMissing = errors.New("agePublicKey is not set")

var envVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateEnvironmentName checks that environment is prod or staging, the
// only two Environments the Platform repository layout has.
func ValidateEnvironmentName(environment string) error {
	for _, e := range Environments {
		if environment == e {
			return nil
		}
	}
	return fmt.Errorf("%w: %q must be prod or staging", ErrInvalidEnvironment, environment)
}

// ValidateSecretKey checks that key can be a secret's KEY: a POSIX
// environment variable name (letters, digits and underscores, not starting
// with a digit). It also becomes part of a Secret name and a file name, by
// way of SecretSlug, which is why it must not be empty.
func ValidateSecretKey(key string) error {
	if !envVarName.MatchString(key) {
		return fmt.Errorf("%w: %q must be letters, digits and underscores, and not start with a digit", ErrInvalidSecretKey, key)
	}
	return nil
}

// chartFullname is the object name the chart gives every Environment's
// objects (_helpers.tpl's application.fullname): the bare Application name
// for prod, <name>-staging for staging. A Secret this ticket writes takes
// the same shape, <fullname>-<key-slug>, so prod and staging never collide
// in a shared namespace.
func chartFullname(application, environment string) string {
	if environment == "prod" {
		return application
	}
	return application + "-" + environment
}

// SecretSlug is the key-slug part of a secret's Secret name and file name:
// key lowercased with underscores turned to dashes, so API_KEY becomes
// api-key. Only meaningful for a key ValidateSecretKey accepts.
func SecretSlug(key string) string {
	return strings.ToLower(strings.ReplaceAll(key, "_", "-"))
}

// Secret is one KEY=value pair to encrypt into an Environment of the
// Platform repository.
type Secret struct {
	Key   string
	Value string
}

// SetSecrets encrypts each of secrets with the Platform's age public key and
// writes one SOPS-encrypted Kubernetes Secret per key under
// applications/<application>/<environment>/sops/, in the KSOPS layout
// docs/platform-repository.md describes: it clones main, checks the
// Environment exists and platform.yaml sets agePublicKey, writes the
// encrypted documents and updates kustomization.yaml, ksops.yaml,
// values.yaml and application.yaml, commits and pushes. A push refused
// because main moved is retried once from a fresh clone, exactly as
// CreateApplication does.
func (w *Writer) SetSecrets(ctx context.Context, application, environment string, secrets []Secret) (Result, error) {
	return runWithRetry(ctx, func(ctx context.Context, _ bool) (Result, error) {
		return w.attemptSetSecrets(ctx, application, environment, secrets)
	})
}

func (w *Writer) attemptSetSecrets(ctx context.Context, application, environment string, secrets []Secret) (Result, error) {
	dir, err := os.MkdirTemp("", "iidp-platform-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)

	repo, err := git.Clone(ctx, w.URL, Branch, dir, w.Auth)
	if err != nil {
		return Result{}, fmt.Errorf("cloning %s: %w", platform.Repository, err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		return Result{}, err
	}
	if cfg.AgePublicKey == "" {
		return Result{}, fmt.Errorf("%w: %s in %s sets no agePublicKey; iidp secret set cannot encrypt without it", ErrAgePublicKeyMissing, ConfigFile, platform.Repository)
	}

	envDir := EnvironmentDir(application, environment)
	switch _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(envDir))); {
	case errors.Is(err, fs.ErrNotExist):
		return Result{}, fmt.Errorf("%w: %s has no %s Environment for %q (expected %s/); create it with iidp app create first", ErrEnvironmentMissing, platform.Repository, environment, application, envDir)
	case err != nil:
		return Result{}, fmt.Errorf("checking for the Environment: %w", err)
	}

	files, err := w.writeSecrets(ctx, dir, application, environment, envDir, cfg.AgePublicKey, secrets)
	if err != nil {
		return Result{}, err
	}
	if err := repo.Add(ctx, files...); err != nil {
		return Result{}, err
	}
	keys := make([]string, len(secrets))
	for i, s := range secrets {
		keys[i] = s.Key
	}
	if err := repo.Commit(ctx, "iidp secret set "+application+" "+environment+" "+strings.Join(keys, " ")); err != nil {
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
		return Result{}, fmt.Errorf("pushing to %s: %w\nIf this is a permission error, ask the Platform admin for write access to %s", platform.Repository, err, platform.Repository)
	}
	return Result{Config: cfg, Files: files}, nil
}

// writeSecrets encrypts and writes each secret's document, then updates the
// Environment's kustomization.yaml, ksops.yaml, values.yaml and
// application.yaml, returning every path written or changed, relative to
// the Platform repository root, in a fixed order.
func (w *Writer) writeSecrets(ctx context.Context, dir, application, environment, envDir, agePublicKey string, secrets []Secret) ([]string, error) {
	fullname := chartFullname(application, environment)
	sopsDir := path.Join(envDir, "sops")
	sopsAbs := filepath.Join(dir, filepath.FromSlash(sopsDir))
	if err := os.MkdirAll(sopsAbs, 0o755); err != nil {
		return nil, err
	}

	encryptor := w.encryptor()
	var files []string
	secretNames := make([]string, 0, len(secrets))
	for _, s := range secrets {
		slug := SecretSlug(s.Key)
		secretName := fullname + "-" + slug
		plaintext, err := render.SecretDocument(render.Secret{
			Name:        secretName,
			Application: application,
			Environment: environment,
			Key:         s.Key,
			Value:       s.Value,
		})
		if err != nil {
			return nil, err
		}
		encRelPath := path.Join(sopsDir, slug+".enc.yaml")
		encrypted, err := encryptor.Encrypt(ctx, plaintext, agePublicKey, encRelPath)
		if err != nil {
			return nil, fmt.Errorf("encrypting %s: %w", s.Key, err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(encRelPath)), encrypted, 0o644); err != nil {
			return nil, err
		}
		files = append(files, encRelPath)
		secretNames = append(secretNames, secretName)
	}

	kustRelPath := path.Join(sopsDir, "kustomization.yaml")
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(kustRelPath)), render.SopsKustomization(), 0o644); err != nil {
		return nil, err
	}
	files = append(files, kustRelPath)

	entries, err := os.ReadDir(sopsAbs)
	if err != nil {
		return nil, err
	}
	var encFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".enc.yaml") {
			encFiles = append(encFiles, e.Name())
		}
	}
	ksopsRelPath := path.Join(sopsDir, "ksops.yaml")
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(ksopsRelPath)), render.KsopsGenerator(fullname+"-secrets", encFiles), 0o644); err != nil {
		return nil, err
	}
	files = append(files, ksopsRelPath)

	valuesRelPath := path.Join(envDir, "values.yaml")
	valuesAbs := filepath.Join(dir, filepath.FromSlash(valuesRelPath))
	valuesData, err := os.ReadFile(valuesAbs)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", valuesRelPath, err)
	}
	valuesChanged := false
	for _, name := range secretNames {
		var changed bool
		valuesData, changed, err = render.AddSecretName(valuesData, name)
		if err != nil {
			return nil, err
		}
		valuesChanged = valuesChanged || changed
	}
	if valuesChanged {
		if err := os.WriteFile(valuesAbs, valuesData, 0o644); err != nil {
			return nil, err
		}
		files = append(files, valuesRelPath)
	}

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

	return files, nil
}
