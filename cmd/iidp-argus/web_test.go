package main

import (
	"encoding/json"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/argus"
)

// The page Argus embeds loads nothing from outside Argus: every file
// index.html names and every module any module imports is in the embedded web
// directory. The server's Content-Security-Policy enforces the same in the
// browser; this catches a broken or external reference before it gets that far.
func TestThePageLoadsNothingFromOutside(t *testing.T) {
	files, err := fs.Sub(web, "web")
	if err != nil {
		t.Fatal(err)
	}
	exists := func(p string) bool {
		_, err := fs.Stat(files, strings.TrimPrefix(path.Clean(p), "/"))
		return err == nil
	}
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		t.Fatal(err)
	}

	// index.html's own references.
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"]*)"`).FindAllStringSubmatch(string(index), -1) {
		if ref := m[1]; strings.Contains(ref, ":") || strings.HasPrefix(ref, "//") || !exists(ref) {
			t.Errorf("index.html refers to %q, which is not in the web directory", ref)
		}
	}

	// The import map.
	m := regexp.MustCompile(`(?s)<script type="importmap">(.*?)</script>`).FindStringSubmatch(string(index))
	if m == nil {
		t.Fatal("index.html has no import map")
	}
	var importMap struct {
		Imports map[string]string `json:"imports"`
	}
	if err := json.Unmarshal([]byte(m[1]), &importMap); err != nil {
		t.Fatalf("the import map: %v", err)
	}
	for name, target := range importMap.Imports {
		if !strings.HasPrefix(target, "./") || !exists(target) {
			t.Errorf("the import map maps %q to %q, which is not in the web directory", name, target)
		}
	}
	resolve := func(from, spec string) (string, bool) {
		if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") || strings.HasPrefix(spec, "/") {
			if strings.HasPrefix(spec, "/") {
				return spec, true
			}
			return path.Join(path.Dir(from), spec), true
		}
		for name, target := range importMap.Imports {
			if spec == name {
				return target, true
			}
			if strings.HasSuffix(name, "/") && strings.HasPrefix(spec, name) {
				return target + strings.TrimPrefix(spec, name), true
			}
		}
		return "", false
	}

	// Every module's imports, the vendored ones' included; and no
	// stylesheet reaches out either.
	imports := regexp.MustCompile(`(?m)(?:^\s*import\s[^'"]*?|^\s*\}\s*from\s|\bexport\s[^'"]*?from\s|\bimport\()\s*['"]([^'"]+)['"]`)
	modules := 0
	err = fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(files, p)
		if err != nil {
			return err
		}
		switch path.Ext(p) {
		case ".js":
			modules++
			for _, m := range imports.FindAllStringSubmatch(string(data), -1) {
				target, ok := resolve(p, m[1])
				if !ok || !exists(target) {
					t.Errorf("%s imports %q, which is not in the web directory", p, m[1])
				}
			}
		case ".css":
			if regexp.MustCompile(`@import|url\(\s*['"]?(?:https?:)?//`).Match(data) {
				t.Errorf("%s loads something from outside", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if modules < 20 {
		t.Errorf("found %d modules; the walk missed the page", modules)
	}

	// And the policy the server sends allows the import map by its hash.
	if csp := argus.ContentSecurityPolicy(files); !strings.Contains(csp, "script-src 'self' 'sha256-") {
		t.Errorf("Content-Security-Policy = %q, want the import map's hash", csp)
	}
}
