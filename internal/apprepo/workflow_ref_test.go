package apprepo

import "testing"

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
