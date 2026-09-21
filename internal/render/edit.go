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
