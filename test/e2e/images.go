package e2e

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

// LogImageSources logs, from the kubelet's events, which images the node
// pulled from a registry and which it found already present. Events of a
// namespace that has since been deleted are gone with it. Best-effort, like
// the other diagnostics: it never fails the test.
func (c *Cluster) LogImageSources(ctx context.Context) {
	out, err := c.Kubectl(ctx, "get", "events", "-A", "--field-selector=reason=Pulled", "-o", "json")
	if err != nil {
		c.Log("image sources: kubectl get events failed: %v\n%s", err, out)
		return
	}
	var events struct {
		Items []struct {
			Message string `json:"message"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &events); err != nil {
		c.Log("image sources: parse events: %v", err)
		return
	}
	pulled, present := map[string]bool{}, map[string]bool{}
	for _, e := range events.Items {
		image := quoted(e.Message)
		switch {
		case image == "":
		case strings.HasPrefix(e.Message, "Successfully pulled"):
			pulled[image] = true
		case strings.Contains(e.Message, "already present on machine"):
			present[image] = true
		}
	}
	c.Log("images the node pulled from a registry:\n  %s", strings.Join(sortedKeys(pulled), "\n  "))
	c.Log("images the node found already present:\n  %s", strings.Join(sortedKeys(present), "\n  "))
}

// quoted returns the first double-quoted string in s, or "".
func quoted(s string) string {
	start := strings.IndexByte(s, '"')
	if start < 0 {
		return ""
	}
	end := strings.IndexByte(s[start+1:], '"')
	if end < 0 {
		return ""
	}
	return s[start+1 : start+1+end]
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
