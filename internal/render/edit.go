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
// app create writes, docs/implementation-notes/13-cli-capabilities.md).
// postgres.migrationCommand is left as it is: the Deploy gate writes it,
// from the Application repository's iidp.yaml, with the image it belongs
// to (docs/implementation-notes/66-migration-command-in-repo.md).
// Everything else in the document -- comments, key order, other values,
// secrets -- is preserved. Callers refuse the Capability when it is already
// enabled, so this always has something to change.
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

// EnableLogin edits an Environment's values.yaml in place to turn the Itema
// login Capability on for cookieDomain: login.enabled: true and
// platform.loginCookieDomain: cookieDomain. On an Environment that already
// has login on it only (re)writes the cookie domain, which is how a custom
// domain added to such an Environment, perhaps written before the field
// existed, gets the value the chart checks it against
// (docs/implementation-notes/76-login-in-zone-domains.md).
func EnableLogin(valuesYAML []byte, cookieDomain string) ([]byte, error) {
	root, err := decodeDocument(valuesYAML, "values.yaml")
	if err != nil {
		return nil, err
	}
	setNestedValue(root, []string{"login", "enabled"}, boolNode(true))
	setNestedValue(root, []string{"platform", "loginCookieDomain"}, scalarNode(cookieDomain))
	return encodeDocument(root)
}

// SetLoginGroups edits an Environment's values.yaml in place to make
// login.groups exactly groups (an empty list when there are none), and
// reports whether it changed anything. The list is replaced, not merged:
// iidp app add-capability --login-group gives the whole new list
// (docs/implementation-notes/92-sign-in-groups.md). Everything else in the
// document is preserved.
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

// CopyValuesForStaging turns a prod Environment's values.yaml (after any
// Capability edits made in the same iidp app add-capability run) into the
// starting values.yaml of a new staging Environment: the same Kind, image,
// size, port, probe, plain env and Postgres settings, but environment:
// staging, no image tag yet (nothing has deployed there), no custom domains
// (they apply to prod only, docs/implementation-notes/13-cli-capabilities.md),
// no Scheduled tasks (staging's first deploy brings its own), and no
// secrets list -- prod's Secrets are not copied, since the CLI would
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
	// Staging's Scheduled tasks are those of the commit its first deploy
	// brings, not prod's.
	removeMappingKey(root, "tasks")
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

// SetImageTag sets image.tag in a values.yaml document (written by
// iidp app create, later edited in place by iidp ci set-image) to tag,
// leaving every other key, and every comment, untouched. It reports
// whether the tag actually changed.
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
// document to command, leaving every other key and comment untouched, and
// reports whether it changed anything. "" clears the command (an explicit
// "", the value app create writes). A non-empty command on an Environment
// without postgres.enabled: true is refused with ErrNoPostgres; clearing is
// always allowed, and is a no-op where there is nothing to clear.
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

// ErrTasksOnStaticSite is returned by SetTasks for tasks on an Environment
// whose kind is static-site: a Static site's image is nginx serving files,
// with no command of the Application's to run, and the chart would refuse
// to render them.
var ErrTasksOnStaticSite = errors.New("a Static site cannot run Scheduled tasks")

// SetTasks sets the top-level tasks list of a values.yaml document to
// tasks, leaving every other key and comment untouched, and reports whether
// it changed anything. No tasks removes the key, so an Environment that
// never had tasks keeps a values file without one. Tasks on a Static site
// (kind: static-site) are refused with ErrTasksOnStaticSite; removing is
// always allowed.
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
			// Every value a string scalar, whatever it looks like: yaml
			// quotes a schedule (it starts with a digit or *) as needed.
			item.Content = append(item.Content, scalarNode(field[0]), &yaml.Node{Kind: yaml.ScalarNode, Value: field[1], Tag: "!!str"})
		}
		list.Content = append(list.Content, item)
	}
	setNestedValue(root, []string{"tasks"}, list)
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
