// Package migrate detects the migration command a Postgres Capability
// should run before every rollout, by looking for Prisma, Drizzle or an npm
// "migrate" script in an Application repository. The commands are the ones
// each tool documents for running migrations in production.
package migrate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Detection is a migration command Detect found.
type Detection struct {
	// Tool names what was found, for example "Prisma (prisma/schema.prisma)".
	Tool string
	// Command is the shell line suggested for postgres.migrationCommand.
	Command string
}

// Detect looks in dir, in order, for a Prisma schema, a Drizzle config or an
// npm "migrate" script, and returns the first match. A missing dir is not an
// error: it reports false, like a dir with none of them.
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
		hasMigrateScript, err := npmHasMigrateScript(dir)
		if err != nil {
			return Detection{}, false, err
		}
		if hasMigrateScript {
			return Detection{Tool: "an npm migrate script", Command: "npm run migrate"}, true, nil
		}
	}
	return Detection{}, false, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func npmHasMigrateScript(dir string) (bool, error) {
	path := filepath.Join(dir, "package.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false, fmt.Errorf("parsing %s: %w", path, err)
	}
	_, ok := pkg.Scripts["migrate"]
	return ok, nil
}
