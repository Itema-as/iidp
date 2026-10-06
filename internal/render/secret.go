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
	Name        string
	Application string
	Environment string
	Key         string
	Value       string
}

// SecretDocument renders the plaintext Kubernetes Secret manifest for s.
func SecretDocument(s Secret) ([]byte, error) {
	return yaml.Marshal(secretDocument(s.Name, s.Application, s.Environment, "Opaque", map[string]string{s.Key: s.Value}))
}

// PasswordSecret is the plaintext password Secret of one database access
// role, before encryption. Like Secret, never write it to disk.
type PasswordSecret struct {
	Name        string
	Application string
	Environment string
	// Username is the role's name, which CloudNativePG checks against the
	// role it sets the password of.
	Username string
	Password string
}

// PasswordSecretDocument renders the plaintext Kubernetes Secret manifest
// for s in the shape a CloudNativePG managed role's passwordSecret takes:
// kubernetes.io/basic-auth, with username and password. The cnpg.io/reload
// label makes CloudNativePG apply a changed password at once rather than
// at its next reconciliation.
func PasswordSecretDocument(s PasswordSecret) ([]byte, error) {
	doc := secretDocument(s.Name, s.Application, s.Environment, "kubernetes.io/basic-auth", map[string]string{"username": s.Username, "password": s.Password})
	doc.Metadata.Labels["cnpg.io/reload"] = "true"
	return yaml.Marshal(doc)
}

// secretDocument is the Secret every Platform secret is written as.
// needs-hash is off so the name stays what values.yaml and ksops.yaml
// reference, and sync-wave -2 makes the Secret exist a wave before the
// migration Job that may need it.
func secretDocument(name, application, environment, secretType string, stringData map[string]string) secretDoc {
	return secretDoc{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: secretMetadata{
			Name: name,
			Labels: map[string]string{
				"app.kubernetes.io/name":    application,
				"iidp.itema.no/application": application,
				"iidp.itema.no/environment": environment,
			},
			Annotations: map[string]string{
				"kustomize.config.k8s.io/needs-hash": "false",
				"argocd.argoproj.io/sync-wave":       "-2",
			},
		},
		Type:       secretType,
		StringData: stringData,
	}
}

type secretDoc struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   secretMetadata `yaml:"metadata"`
	Type       string         `yaml:"type"`
	// StringData holds one Secret's whole content: the CLI has no private
	// key, so it cannot merge into an existing encrypted document.
	StringData map[string]string `yaml:"stringData"`
}

type secretMetadata struct {
	Name        string            `yaml:"name"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

// SopsKustomization is the kustomization.yaml every Environment's sops/
// directory gets.
func SopsKustomization() []byte {
	return []byte("apiVersion: kustomize.config.k8s.io/v1beta1\n" +
		"kind: Kustomization\n" +
		"generators:\n" +
		"  - ksops.yaml\n")
}

// KsopsGenerator renders the KSOPS generator for an Environment's sops/
// directory, listing every encrypted file name, sorted.
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
	// doc's shape is fixed, so it always marshals.
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
