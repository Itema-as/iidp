package render

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// AddSecretName ensures the top-level secrets: list of a values.yaml
// document contains name, adding the key fresh if it is not there yet or
// appending to the existing list, and reports whether it changed anything.
// Everything else in the document (comments, key order, existing entries)
// is left as it was.
func AddSecretName(valuesYAML []byte, name string) (out []byte, changed bool, err error) {
	root, err := decodeDocument(valuesYAML, "values.yaml")
	if err != nil {
		return nil, false, err
	}
	if seq := mappingValue(root, "secrets"); seq != nil {
		for _, item := range seq.Content {
			if item.Value == name {
				return valuesYAML, false, nil
			}
		}
		seq.Content = append(seq.Content, scalarNode(name))
	} else {
		root.Content = append(root.Content,
			scalarNode("secrets"),
			&yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{scalarNode(name)}},
		)
	}
	out, err = encodeDocument(root)
	return out, true, err
}

// AddKustomizeSource ensures the ArgoCD Application document
// applicationYAML has a source for path (a kustomize/ksops directory) in
// the Platform repository at repoURL on main, adding it after the existing
// sources when it is not already there. It matches an existing source by
// path alone, since only one source in an Environment's Application ever
// sets one.
func AddKustomizeSource(applicationYAML []byte, repoURL, path string) (out []byte, changed bool, err error) {
	root, err := decodeDocument(applicationYAML, "application.yaml")
	if err != nil {
		return nil, false, err
	}
	spec := mappingValue(root, "spec")
	if spec == nil {
		return nil, false, fmt.Errorf("application.yaml has no spec")
	}
	sources := mappingValue(spec, "sources")
	if sources == nil {
		return nil, false, fmt.Errorf("application.yaml has no spec.sources")
	}
	for _, src := range sources.Content {
		if p := mappingValue(src, "path"); p != nil && p.Value == path {
			return applicationYAML, false, nil
		}
	}
	sources.Content = append(sources.Content, &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarNode("repoURL"), scalarNode(repoURL),
			scalarNode("targetRevision"), scalarNode("main"),
			scalarNode("path"), scalarNode(path),
		},
	})
	out, err = encodeDocument(root)
	return out, true, err
}

// EnablePostgres edits an Environment's values.yaml in place to turn the
// Postgres Capability on: postgres.enabled: true, platform.backupsBucket
// and platform.objectStorageEndpoint from platform.yaml (the same fields
// app create writes, docs/implementation-notes/13-cli-capabilities.md), and
// postgres.migrationCommand when migrationCommand is not empty (an empty
// migrationCommand leaves whatever was already there, "" on a freshly
// created Environment, untouched). Everything else in the document --
// comments, key order, other values, secrets -- is preserved. Callers
// refuse the Capability when it is already enabled, so this always has
// something to change.
func EnablePostgres(valuesYAML []byte, migrationCommand, backupsBucket, objectStorageEndpoint string) ([]byte, error) {
	root, err := decodeDocument(valuesYAML, "values.yaml")
	if err != nil {
		return nil, err
	}
	setNestedValue(root, []string{"postgres", "enabled"}, boolNode(true))
	if migrationCommand != "" {
		setNestedValue(root, []string{"postgres", "migrationCommand"}, scalarNode(migrationCommand))
	}
	setNestedValue(root, []string{"platform", "backupsBucket"}, scalarNode(backupsBucket))
	setNestedValue(root, []string{"platform", "objectStorageEndpoint"}, scalarNode(objectStorageEndpoint))
	return encodeDocument(root)
}

// SetSize edits an Environment's values.yaml in place to set the top-level
// size key, and reports whether it changed anything.
func SetSize(valuesYAML []byte, size string) (out []byte, changed bool, err error) {
	root, err := decodeDocument(valuesYAML, "values.yaml")
	if err != nil {
		return nil, false, err
	}
	if existing := mappingValue(root, "size"); existing != nil && existing.Value == size {
		return valuesYAML, false, nil
	}
	setNestedValue(root, []string{"size"}, scalarNode(size))
	out, err = encodeDocument(root)
	return out, true, err
}

// AddDomain appends host to the domains: list of a values.yaml document
// (creating the key if somehow absent) unless it is already there, and
// reports whether it changed anything. The same shape as AddSecretName, one
// level simpler since domains has no per-item structure.
func AddDomain(valuesYAML []byte, host string) (out []byte, changed bool, err error) {
	root, err := decodeDocument(valuesYAML, "values.yaml")
	if err != nil {
		return nil, false, err
	}
	seq := mappingValue(root, "domains")
	if seq == nil {
		seq = &yaml.Node{Kind: yaml.SequenceNode}
		root.Content = append(root.Content, scalarNode("domains"), seq)
	}
	for _, item := range seq.Content {
		if item.Value == host {
			return valuesYAML, false, nil
		}
	}
	seq.Content = append(seq.Content, scalarNode(host))
	out, err = encodeDocument(root)
	return out, true, err
}

// CopyValuesForStaging turns a prod Environment's values.yaml (after any
// Capability edits made in the same iidp app add-capability run) into the
// starting values.yaml of a new staging Environment: the same Kind, image,
// size, port, probe, plain env and Postgres settings, but environment:
// staging, no image tag yet (nothing has deployed there), no custom domains
// (they apply to prod only, docs/implementation-notes/13-cli-capabilities.md),
// and no secrets list -- prod's Secrets are not copied, since the CLI would
// have to read them back out of the cluster's age-encrypted documents to do
// so, which it cannot; secretsDropped reports whether prod had any, so the
// caller can tell the developer to run iidp secret set again for staging.
func CopyValuesForStaging(prodValuesYAML []byte) (out []byte, secretsDropped bool, err error) {
	root, err := decodeDocument(prodValuesYAML, "values.yaml")
	if err != nil {
		return nil, false, err
	}
	setNestedValue(root, []string{"environment"}, scalarNode("staging"))
	// An explicitly double-quoted empty string, the same style
	// yaml.Marshal gives image.tag in render.Values: an unstyled empty
	// scalar would round-trip as null, not "".
	setNestedValue(root, []string{"image", "tag"}, &yaml.Node{Kind: yaml.ScalarNode, Value: "", Style: yaml.DoubleQuotedStyle})
	if domains := mappingValue(root, "domains"); domains != nil {
		domains.Content = nil
	}
	secretsDropped = removeMappingKey(root, "secrets")
	out, err = encodeDocument(root)
	return out, secretsDropped, err
}

// setNestedValue ensures the mapping at path (a sequence of keys, each
// nested one level deeper than the last) holds value, creating intermediate
// mapping nodes when they are missing, and replacing whatever scalar was
// there otherwise. Every helper above uses it instead of walking
// mappingValue by hand, the same way AddSecretName and AddKustomizeSource
// already read a values.yaml or application.yaml document's nodes in place.
func setNestedValue(root *yaml.Node, path []string, value *yaml.Node) {
	node := root
	for i, key := range path {
		if i == len(path)-1 {
			if existing := mappingValue(node, key); existing != nil {
				*existing = *value
			} else {
				node.Content = append(node.Content, scalarNode(key), value)
			}
			return
		}
		child := mappingValue(node, key)
		if child == nil {
			child = &yaml.Node{Kind: yaml.MappingNode}
			node.Content = append(node.Content, scalarNode(key), child)
		}
		node = child
	}
}

// removeMappingKey deletes key from mapping if present and reports whether
// it did.
func removeMappingKey(mapping *yaml.Node, key string) bool {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return true
		}
	}
	return false
}

func boolNode(v bool) *yaml.Node {
	value := "false"
	if v {
		value = "true"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Value: value, Tag: "!!bool"}
}

// decodeDocument parses data as a YAML document and returns its top-level
// mapping node, the node every helper here edits in place.
func decodeDocument(data []byte, what string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", what, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s is not a YAML mapping", what)
	}
	return doc.Content[0], nil
}

// mappingValue returns the value node for key in mapping, or nil when
// mapping has no such key.
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func scalarNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: v}
}

// encodeDocument re-serialises root (a mapping node holding the document's
// comments) with the same 4-space indent render's other Marshal calls use.
func encodeDocument(root *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(4)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
