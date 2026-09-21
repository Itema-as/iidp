// Package migrate detects the migration command a Postgres Capability
// should run before every rollout, by looking for known migration tooling
// in an Application repository: Prisma, Drizzle or an npm "migrate"
// script. docs/design.md ("The wizard") names the three; the commands
// themselves (npx prisma migrate deploy, npx drizzle-kit migrate) match
// each tool's own documentation for running migrations in production.
package migrate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Detection is a migration command Detect found, and what triggered it, for
// the message shown to the developer.
type Detection struct {
	// Tool names what was found, for example "Prisma (prisma/schema.prisma)".
	Tool string
	// Command is the shell line suggested for postgres.migrationCommand.
	Command string
}

// Detect looks in dir, in order, for a Prisma schema, a Drizzle config or an
// npm "migrate" script in package.json, and returns the first match. ok is
// false when dir has none of them, including when dir does not exist: a
// missing directory is not an error here, since callers may probe one that
// turns out to have nothing (a freshly generated Create template, an
// --app-dir the developer got wrong).
func Detect(dir string) (Detection, bool, error) {
	if dir == "" {
		return Detection{}, false, nil
	}
	for _, name := range []string{"prisma/schema.prisma", "schema.prisma"} {
		if fileExists(filepath.Join(dir, name)) {
			return Detection{Tool: "Prisma (" + name + ")", Command: "npx prisma migrate deploy"}, true, nil
		}
	}
	matches, err := filepath.Glob(filepath.Join(dir, "drizzle.config.*"))
	if err != nil {
		return Detection{}, false, err
	}
	if len(matches) > 0 {
		return Detection{Tool: "Drizzle (" + filepath.Base(matches[0]) + ")", Command: "npx drizzle-kit migrate"}, true, nil
	}
	if fileExists(filepath.Join(dir, "package.json")) {
		script, ok, err := npmMigrateScript(dir)
		if err != nil {
			return Detection{}, false, err
		}
		if ok {
			_ = script
			return Detection{Tool: "an npm migrate script", Command: "npm run migrate"}, true, nil
		}
	}
	return Detection{}, false, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// npmMigrateScript reports whether dir's package.json has a "migrate"
// script, and its command.
func npmMigrateScript(dir string) (command string, ok bool, err error) {
	path := filepath.Join(dir, "package.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", false, fmt.Errorf("parsing %s: %w", path, err)
	}
	command, ok = pkg.Scripts["migrate"]
	return command, ok, nil
}
