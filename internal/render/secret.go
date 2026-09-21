package render

import (
	"sort"

	"gopkg.in/yaml.v3"
)

// Secret is the plaintext Kubernetes Secret document for one key of one
// Platform secret, before encryption. Never write this to disk: pipe it
// straight into an internal/sops.Encryptor.
type Secret struct {
	// Name is the Secret's object name: <fullname>-<key-slug>.
	Name string
	// Application and Environment are the identity labels every object of
	// the Environment carries.
	Application string
	Environment string
	// Key and Value are the single entry the Secret's stringData holds.
	Key   string
	Value string
}

// SecretDocument renders the plaintext Kubernetes Secret manifest for s. The
// annotations are what the migration Job's sync-wave ordering needs (see
// notes for #7): needs-hash off, so the name stays what values.yaml and
// ksops.yaml reference, and sync-wave -2 so the Secret exists a wave before
// the migration Job that may need it.
func SecretDocument(s Secret) ([]byte, error) {
	doc := secretDoc{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: secretMetadata{
			Name: s.Name,
			Labels: map[string]string{
				"app.kubernetes.io/name":    s.Application,
				"iidp.itema.no/application": s.Application,
				"iidp.itema.no/environment": s.Environment,
			},
			Annotations: map[string]string{
				"kustomize.config.k8s.io/needs-hash": "false",
				"argocd.argoproj.io/sync-wave":       "-2",
			},
		},
		Type:       "Opaque",
		StringData: map[string]string{s.Key: s.Value},
	}
	return yaml.Marshal(doc)
}

type secretDoc struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   secretMetadata `yaml:"metadata"`
	Type       string         `yaml:"type"`
	// StringData holds exactly one key: one SOPS document per key, because
	// the CLI has no private key and cannot merge into an existing
	// encrypted document (docs/platform-repository.md, "Secrets").
	StringData map[string]string `yaml:"stringData"`
}

type secretMetadata struct {
	Name        string            `yaml:"name"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

// SopsKustomization is the kustomization.yaml every Environment's sops/
// directory gets: a generator pointing at ksops.yaml, the same shape
// bootstrap/sops uses.
func SopsKustomization() []byte {
	return []byte("apiVersion: kustomize.config.k8s.io/v1beta1\n" +
		"kind: Kustomization\n" +
		"generators:\n" +
		"  - ksops.yaml\n")
}

// KsopsGenerator renders the KSOPS generator for an Environment's sops/
// directory, listing every encrypted file name. files is sorted so the
// output is deterministic regardless of directory listing order.
func KsopsGenerator(name string, files []string) []byte {
	sorted := append([]string(nil), files...)
	sort.Strings(sorted)
	doc := ksopsDoc{
		APIVersion: "viaduct.ai/v1",
		Kind:       "ksops",
		Metadata: ksopsMetadata{
			Name: name,
			Annotations: map[string]string{
				"config.kubernetes.io/function": "exec:\n  path: ksops\n",
			},
		},
		Files: sorted,
	}
	// doc's shape is fixed and always marshals; yaml.Marshal only fails on
	// unsupported Go types.
	out, err := yaml.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return out
}

type ksopsDoc struct {
	APIVersion string        `yaml:"apiVersion"`
	Kind       string        `yaml:"kind"`
	Metadata   ksopsMetadata `yaml:"metadata"`
	Files      []string      `yaml:"files"`
}

type ksopsMetadata struct {
	Name        string            `yaml:"name"`
	Annotations map[string]string `yaml:"annotations"`
}
