// Package appconfig is iidp.yaml, the file at the root of an Application
// repository that holds the settings the Platform takes from the code
// rather than from the Platform repository, because they must change
// together with the code: today only the migration command
// (docs/implementation-notes/66-migration-command-in-repo.md).
//
// iidp ci set-image reads it from the checkout of the commit being deployed
// or promoted and sends it to the Deploy gate with the image tag, so an
// Environment always runs the migration command of the code it runs.
//
// What the file's presence means is part of the contract:
//
//   - No iidp.yaml: the deploy sends no migration command, and the gate
//     leaves the Environment's postgres.migrationCommand as it is. An
//     Application made before the file existed keeps working unchanged.
//   - iidp.yaml without migrationCommand, or with an empty one: the deploy
//     sends "", and the gate clears the Environment's command. Deleting the
//     line is how a developer says "no migration".
//   - iidp.yaml with migrationCommand: the gate sets it.
package appconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// FileName is the file's name, at the root of the Application repository.
const FileName = "iidp.yaml"

// MaxMigrationCommandBytes is the longest migration command the Deploy gate
// accepts. The command is one shell line run with sh -c; anything longer
// belongs in a script shipped in the image.
const MaxMigrationCommandBytes = 1024

// File is iidp.yaml's content.
type File struct {
	// MigrationCommand is the shell line the migration Job runs before
	// every rollout, with DATABASE_URL set. "" means no migration.
	MigrationCommand string
}

// Read reads FileName from dir. ok is false when dir has no such file.
// Only known keys are accepted, each with the type it must have, so a
// misspelt key fails the deploy instead of silently clearing the
// migration command. Surrounding whitespace is trimmed, so a folded
// block scalar (>) reads as the one line it folds to.
func Read(dir string) (f File, ok bool, err error) {
	path := filepath.Join(dir, FileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// iidp.yml would otherwise be ignored without a word, and the
		// command in it never sent.
		if _, statErr := os.Stat(filepath.Join(dir, "iidp.yml")); statErr == nil {
			return File{}, false, fmt.Errorf("found iidp.yml, which iidp does not read: rename it to %s", FileName)
		}
		return File{}, false, nil
	}
	if err != nil {
		return File{}, false, fmt.Errorf("reading %s: %w", FileName, err)
	}
	f, err = Parse(data)
	if err != nil {
		return File{}, false, err
	}
	return f, true, nil
}

// Parse parses iidp.yaml's content: a YAML mapping (or an empty document,
// only comments) whose only key is migrationCommand, a string or null.
func Parse(data []byte) (File, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return File{}, fmt.Errorf("%s is not valid YAML: %w", FileName, err)
	}
	var f File
	if len(doc.Content) == 0 {
		return f, nil
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return f, nil
	}
	if root.Kind != yaml.MappingNode {
		return File{}, fmt.Errorf("%s must be a mapping of settings, such as migrationCommand: npx prisma migrate deploy", FileName)
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if seen[key.Value] {
			return File{}, fmt.Errorf("%s line %d: %s is set twice", FileName, key.Line, key.Value)
		}
		seen[key.Value] = true
		switch key.Value {
		case "migrationCommand":
			if value.Kind != yaml.ScalarNode || (value.Tag != "!!str" && value.Tag != "!!null") {
				return File{}, fmt.Errorf("%s line %d: migrationCommand must be a string, one shell line; quote it if YAML reads it as something else", FileName, value.Line)
			}
			if value.Tag == "!!str" {
				f.MigrationCommand = strings.TrimSpace(value.Value)
			}
		default:
			return File{}, fmt.Errorf("%s line %d: unknown setting %q; the only one is migrationCommand", FileName, key.Line, key.Value)
		}
	}
	if err := ValidateMigrationCommand(f.MigrationCommand); err != nil {
		return File{}, fmt.Errorf("%s: %w", FileName, err)
	}
	return f, nil
}

// ValidateMigrationCommand refuses a migration command that is not one
// shell line of at most MaxMigrationCommandBytes: valid UTF-8, no line
// break or other control character (a tab is fine). "" is valid: it
// means no migration. The Deploy gate applies it to every command it
// receives, and ci set-image before it calls the gate.
func ValidateMigrationCommand(command string) error {
	if len(command) > MaxMigrationCommandBytes {
		return fmt.Errorf("the migration command is %d bytes, more than the %d allowed; put a longer migration in a script in the image and run that", len(command), MaxMigrationCommandBytes)
	}
	if !utf8.ValidString(command) {
		return errors.New("the migration command is not valid UTF-8")
	}
	for _, r := range command {
		switch {
		case r == '\n' || r == '\r':
			return errors.New("the migration command must be one line: it runs with sh -c, so chain steps with &&")
		case r != '\t' && unicode.IsControl(r):
			return fmt.Errorf("the migration command contains the control character %U", r)
		}
	}
	return nil
}

// MigrationCommandLine is the line of iidp.yaml that sets command, quoted
// as YAML needs it: what add-capability --postgres tells the developer
// to add, and what Render writes.
func MigrationCommandLine(command string) string {
	value, err := yaml.Marshal(command)
	if err != nil {
		// A string always marshals.
		panic(err)
	}
	return "migrationCommand: " + strings.TrimSpace(string(value))
}

// exampleCommand is shown commented out in a file that sets no command.
const exampleCommand = "npx prisma migrate deploy"

// Render is the iidp.yaml iidp app create and Adopt write: a comment
// saying what the file is and when and where the migration command runs,
// then the command, or a commented-out example when there is none.
func Render(migrationCommand string) []byte {
	var b strings.Builder
	b.WriteString(`# Settings the Platform takes from this repository rather than from the
# Platform repository, because they change with the code. The deploy
# workflow's iidp ci set-image reads this file from the commit it deploys
# (a push to main) or promotes (a v* tag) and sends it to the Platform's
# Deploy gate together with the image, so an Environment always runs the
# settings of the code it runs. Written by iidp app create; edit it freely.
#
# migrationCommand runs before every rollout of this Application, in each
# Environment it deploys to: a Kubernetes Job in that Environment, from the
# image just built from this commit, with DATABASE_URL and the
# Application's own environment and secrets set. It runs with sh -c, so it
# is one shell line; chain steps with &&. If it fails, the rollout stops and
# the previous version keeps running. It needs the Postgres Capability
# (iidp app add-capability <name> --postgres). Remove the line to run no
# migration; the next deploy clears it. Deleting this whole file instead
# leaves whatever command the Platform already has.
`)
	if migrationCommand == "" {
		b.WriteString("#\n# " + MigrationCommandLine(exampleCommand) + "\n")
	} else {
		b.WriteString(MigrationCommandLine(migrationCommand) + "\n")
	}
	return []byte(b.String())
}
