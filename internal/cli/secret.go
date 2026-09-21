package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
)

// secretSetOptions are the flags and positional arguments of secret set.
type secretSetOptions struct {
	application  string
	environment  string
	pairs        []string
	fromFile     []string
	stdinKeys    []string
	platformRepo string
}

func newSecretCommand(deps Dependencies) *cobra.Command {
	secret := &cobra.Command{
		Use:   "secret",
		Short: "Manage an Application's secrets",
	}
	secret.AddCommand(newSecretSetCommand(deps))
	return secret
}

func newSecretSetCommand(deps Dependencies) *cobra.Command {
	var opts secretSetOptions
	cmd := &cobra.Command{
		Use:   "set <app> <env> [KEY=value ...]",
		Short: "Encrypt and commit secret values for an Application's Environment",
		Long: "Encrypt one or more values with the Platform's age public key (from\n" +
			"platform.yaml) and commit them as SOPS-encrypted Kubernetes Secrets next to\n" +
			"the Environment's values file, where the chart mounts them. The private key\n" +
			"never leaves the cluster; iidp only ever encrypts.\n\n" +
			"A value on the command line (KEY=value) is the simplest form, but it lands\n" +
			"in your shell history. --from-file KEY=path and --stdin KEY avoid that.\n" +
			"At least one KEY, from any combination of the three, is required. Setting an\n" +
			"existing KEY again replaces it; the plaintext is never written to disk and\n" +
			"never appears in the commit.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.application = args[0]
			opts.environment = args[1]
			opts.pairs = args[2:]
			return runSecretSet(cmd, opts, deps)
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&opts.fromFile, "from-file", nil, "KEY=path: read the value for KEY from a file")
	f.StringArrayVar(&opts.stdinKeys, "stdin", nil, "KEY: read the value for KEY from stdin")
	f.StringVar(&opts.platformRepo, "platform-repo", platform.RepositoryURL, "Git URL of the Platform repository")
	_ = f.MarkHidden("platform-repo")
	return cmd
}

func runSecretSet(cmd *cobra.Command, opts secretSetOptions, deps Dependencies) error {
	if err := platformrepo.ValidateName(opts.application); err != nil {
		return err
	}
	if err := platformrepo.ValidateEnvironmentName(opts.environment); err != nil {
		return err
	}
	secrets, err := collectSecrets(cmd, opts)
	if err != nil {
		return err
	}
	if len(secrets) == 0 {
		return fmt.Errorf("at least one KEY=value, --from-file KEY=path or --stdin KEY is required")
	}

	token, err := deps.TokenSource.Token()
	if err != nil {
		return fmt.Errorf("not logged in to GitHub, so %s cannot be written: %w", platform.Repository, err)
	}

	out := cmd.OutOrStdout()
	keys := make([]string, len(secrets))
	for i, s := range secrets {
		keys[i] = s.Key
	}
	fmt.Fprintf(out, "Setting %s for %s (%s): %s\n", keyWord(len(keys)), opts.application, opts.environment, strings.Join(keys, ", "))

	writer := &platformrepo.Writer{
		URL:        opts.platformRepo,
		Auth:       git.Auth{Token: token},
		BeforePush: deps.BeforePush,
	}
	res, err := writer.SetSecrets(cmd.Context(), opts.application, opts.environment, secrets)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\nCommitted to %s:\n", platform.Repository)
	for _, f := range res.Files {
		fmt.Fprintf(out, "  %s\n", f)
	}
	return nil
}

func keyWord(n int) string {
	if n == 1 {
		return "1 secret"
	}
	return fmt.Sprintf("%d secrets", n)
}

// collectSecrets turns the KEY=value positional arguments and the
// --from-file and --stdin flags into the secrets to write, validating every
// KEY and reading every file and stdin before anything is cloned or
// written. It refuses a KEY given more than once across all three sources.
func collectSecrets(cmd *cobra.Command, opts secretSetOptions) ([]platformrepo.Secret, error) {
	if len(opts.stdinKeys) > 1 {
		return nil, fmt.Errorf("--stdin can be given at most once: stdin can only be read once, for %s", strings.Join(opts.stdinKeys, ", "))
	}

	var secrets []platformrepo.Secret
	seen := map[string]bool{}
	add := func(key, value string) error {
		if err := platformrepo.ValidateSecretKey(key); err != nil {
			return err
		}
		if seen[key] {
			return fmt.Errorf("KEY %q is given more than once", key)
		}
		seen[key] = true
		secrets = append(secrets, platformrepo.Secret{Key: key, Value: value})
		return nil
	}

	for _, pair := range opts.pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("%q is not KEY=value", pair)
		}
		if err := add(key, value); err != nil {
			return nil, err
		}
	}
	for _, kv := range opts.fromFile {
		key, path, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("--from-file %q is not KEY=path", kv)
		}
		if err := platformrepo.ValidateSecretKey(key); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("--from-file %s: %w", key, err)
		}
		if err := add(key, trimTrailingNewline(string(data))); err != nil {
			return nil, err
		}
	}
	for _, key := range opts.stdinKeys {
		if err := platformrepo.ValidateSecretKey(key); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, fmt.Errorf("--stdin %s: reading stdin: %w", key, err)
		}
		if err := add(key, trimTrailingNewline(string(data))); err != nil {
			return nil, err
		}
	}
	return secrets, nil
}

// trimTrailingNewline drops one trailing line ending (LF or CRLF), the way
// a file or a piped value from a shell commonly carries one that is not
// part of the intended value.
func trimTrailingNewline(s string) string {
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	return s
}
