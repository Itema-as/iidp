package apprepo

import "testing"

// A caller pins the reusable deploy workflow at the major tag of the iidp
// that wrote it; a dev build, or a version that isn't vX.Y.Z-shaped, gets
// the first major.
func TestMajorTag(t *testing.T) {
	for version, want := range map[string]string{
		"0.2.2":      "v0",
		"v0.2.2":     "v0",
		"1.0.0":      "v1",
		"12.3.4-rc1": "v12",
		"dev":        "v0",
		"":           "v0",
		"x.1.2":      "v0",
	} {
		if got := majorTag(version); got != want {
			t.Errorf("majorTag(%q) = %q, want %q", version, got, want)
		}
	}
}
