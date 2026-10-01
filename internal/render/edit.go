package render

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/appconfig"
)

// AddSecretName ensures the top-level secrets: list of a values.yaml
// document contains name, and reports whether it changed anything. Like
// every editor here, it leaves comments, key order and other entries as
// they were.
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
// the Platform repository at repoURL on main. It matches an existing source
// by path alone, since only one source in an Environment's Application ever
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

// EnablePostgres edits an Environment's values.yaml to turn the Postgres
// Capability on. postgres.migrationCommand is left as it is: the Deploy
// gate writes it from iidp.yaml.
func EnablePostgres(valuesYAML []byte, backupsBucket, objectStorageEndpoint string) ([]byte, error) {
	root, err := decodeDocument(valuesYAML, "values.yaml")
	if err != nil {
		return nil, err
	}
	setNestedValue(root, []string{"postgres", "enabled"}, boolNode(true))
	setNestedValue(root, []string{"platform", "backupsBucket"}, scalarNode(backupsBucket))
	setNestedValue(root, []string{"platform", "objectStorageEndpoint"}, scalarNode(objectStorageEndpoint))
	return encodeDocument(root)
}

// SetSize sets the top-level size key of an Environment's values.yaml, and
// reports whether it changed anything.
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

// AddDomain ensures the domains: list of a values.yaml document contains
// host, and reports whether it changed anything.
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

// EnableLogin edits an Environment's values.yaml to turn the Itema login
// Capability on for cookieDomain. On an Environment that already has login
// on, it only rewrites the cookie domain, which the chart checks custom
// domains against.
func EnableLogin(valuesYAML []byte, cookieDomain string) ([]byte, error) {
	root, err := decodeDocument(valuesYAML, "values.yaml")
	if err != nil {
		return nil, err
	}
	setNestedValue(root, []string{"login", "enabled"}, boolNode(true))
	setNestedValue(root, []string{"platform", "loginCookieDomain"}, scalarNode(cookieDomain))
	return encodeDocument(root)
}

// SetLoginGroups makes login.groups in an Environment's values.yaml exactly
// groups, replacing rather than merging, and reports whether it changed
// anything.
func SetLoginGroups(valuesYAML []byte, groups []string) (out []byte, changed bool, err error) {
	root, err := decodeDocument(valuesYAML, "values.yaml")
	if err != nil {
		return nil, false, err
	}
	if existing := mappingValue(mappingValue(root, "login"), "groups"); existing != nil && existing.Kind == yaml.SequenceNode {
		var current []string
		for _, item := range existing.Content {
			current = append(current, item.Value)
		}
		if slices.Equal(current, groups) {
			return valuesYAML, false, nil
		}
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	if len(groups) == 0 {
		seq.Style = yaml.FlowStyle
	}
	for _, g := range groups {
		seq.Content = append(seq.Content, scalarNode(g))
	}
	setNestedValue(root, []string{"login", "groups"}, seq)
	out, err = encodeDocument(root)
	return out, true, err
}

// CopyValuesForStaging turns a prod Environment's values.yaml into the
// starting values.yaml of a new staging Environment: no image tag, no
// custom domains, no Scheduled tasks (staging's first deploy brings its
// own) and no secrets. The CLI cannot decrypt prod's Secrets to copy them;
// secretsDropped reports whether prod had any.
func CopyValuesForStaging(prodValuesYAML []byte) (out []byte, secretsDropped bool, err error) {
	root, err := decodeDocument(prodValuesYAML, "values.yaml")
	if err != nil {
		return nil, false, err
	}
	setNestedValue(root, []string{"environment"}, scalarNode("staging"))
	// Double-quoted: an unstyled empty scalar would round-trip as null.
	setNestedValue(root, []string{"image", "tag"}, &yaml.Node{Kind: yaml.ScalarNode, Value: "", Style: yaml.DoubleQuotedStyle})
	if domains := mappingValue(root, "domains"); domains != nil {
		domains.Content = nil
	}
	secretsDropped = removeMappingKey(root, "secrets")
	removeMappingKey(root, "tasks")
	// Older values files may carry runAsNonRoot, which the chart doesn't
	// read.
	removeMappingKey(root, "runAsNonRoot")
	out, err = encodeDocument(root)
	return out, secretsDropped, err
}

// setNestedValue sets the value at path, a sequence of nested keys,
// creating intermediate mappings when they are missing.
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

// SetImageTag sets image.tag in a values.yaml document, and reports whether
// it changed.
func SetImageTag(valuesYAML []byte, tag string) (out []byte, changed bool, err error) {
	root, err := decodeDocument(valuesYAML, "values.yaml")
	if err != nil {
		return nil, false, err
	}
	image := mappingValue(root, "image")
	if image == nil {
		return nil, false, fmt.Errorf("values.yaml has no image key")
	}
	for i := 0; i+1 < len(image.Content); i += 2 {
		if image.Content[i].Value != "tag" {
			continue
		}
		value := image.Content[i+1]
		if value.Value == tag {
			return valuesYAML, false, nil
		}
		value.Value = tag
		// Force a plain string scalar: a version like "20260921" or a
		// numeric-looking value must not be re-encoded as a YAML number.
		value.Tag = "!!str"
		value.Style = 0
		out, err = encodeDocument(root)
		return out, true, err
	}
	return nil, false, fmt.Errorf("values.yaml image has no tag key")
}

// ErrNoPostgres is returned by SetMigrationCommand for a command on an
// Environment whose postgres.enabled is not true: there is no database to
// migrate, and the chart would refuse to render it.
var ErrNoPostgres = errors.New("the Postgres Capability is not enabled")

// SetMigrationCommand sets postgres.migrationCommand in a values.yaml
// document, and reports whether it changed anything. "" clears the command.
// A non-empty command without postgres.enabled: true is refused with
// ErrNoPostgres.
func SetMigrationCommand(valuesYAML []byte, command string) (out []byte, changed bool, err error) {
	root, err := decodeDocument(valuesYAML, "values.yaml")
	if err != nil {
		return nil, false, err
	}
	postgres := mappingValue(root, "postgres")
	current := ""
	if existing := mappingValue(postgres, "migrationCommand"); existing != nil && existing.Tag != "!!null" {
		current = existing.Value
	}
	if current == command {
		return valuesYAML, false, nil
	}
	if command != "" {
		if enabled := mappingValue(postgres, "enabled"); enabled == nil || enabled.Value != "true" {
			return nil, false, ErrNoPostgres
		}
	}
	// A string scalar, whatever it looks like; an empty one double-quoted,
	// since an unstyled empty scalar would read back as null.
	value := &yaml.Node{Kind: yaml.ScalarNode, Value: command, Tag: "!!str"}
	if command == "" {
		value.Style = yaml.DoubleQuotedStyle
	}
	setNestedValue(root, []string{"postgres", "migrationCommand"}, value)
	out, err = encodeDocument(root)
	return out, true, err
}

// ErrTasksOnStaticSite is returned by SetTasks for tasks on a Static site,
// whose image is nginx serving files: the chart would refuse to render them.
var ErrTasksOnStaticSite = errors.New("a Static site cannot run Scheduled tasks")

// SetTasks sets the top-level tasks list of a values.yaml document, and
// reports whether it changed anything. No tasks removes the key. Tasks on a
// Static site are refused with ErrTasksOnStaticSite.
func SetTasks(valuesYAML []byte, tasks []appconfig.Task) (out []byte, changed bool, err error) {
	root, err := decodeDocument(valuesYAML, "values.yaml")
	if err != nil {
		return nil, false, err
	}
	var current []appconfig.Task
	if existing := mappingValue(root, "tasks"); existing != nil {
		if err := existing.Decode(&current); err != nil {
			return nil, false, fmt.Errorf("values.yaml tasks: %w", err)
		}
	}
	if slices.Equal(current, tasks) {
		return valuesYAML, false, nil
	}
	if len(tasks) == 0 {
		removeMappingKey(root, "tasks")
		out, err = encodeDocument(root)
		return out, true, err
	}
	if kind := mappingValue(root, "kind"); kind != nil && kind.Value == "static-site" {
		return nil, false, ErrTasksOnStaticSite
	}
	list := &yaml.Node{Kind: yaml.SequenceNode}
	for _, task := range tasks {
		item := &yaml.Node{Kind: yaml.MappingNode}
		for _, field := range [][2]string{{"name", task.Name}, {"schedule", task.Schedule}, {"command", task.Command}} {
			// Tagged !!str, so yaml quotes a value that would otherwise
			// read as another type, such as a schedule starting with *.
			item.Content = append(item.Content, scalarNode(field[0]), &yaml.Node{Kind: yaml.ScalarNode, Value: field[1], Tag: "!!str"})
		}
		list.Content = append(list.Content, item)
	}
	setNestedValue(root, []string{"tasks"}, list)
	out, err = encodeDocument(root)
	return out, true, err
}

// decodeDocument parses data as a YAML document and returns its top-level
// mapping node.
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

// encodeDocument re-serialises root with the same 4-space indent
// yaml.Marshal uses.
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
