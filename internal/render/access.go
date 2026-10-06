package render

import (
	"errors"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

// The database access levels. Each names the lowest permission on the
// Application repository that qualifies for it: admin includes maintain,
// which includes push, which includes pull. AccessNone lets nobody in.
const (
	AccessNone     = "none"
	AccessPull     = "pull"
	AccessPush     = "push"
	AccessMaintain = "maintain"
	AccessAdmin    = "admin"
)

// accessLevels are the levels from least to most permission.
var accessLevels = []string{AccessNone, AccessPull, AccessPush, AccessMaintain, AccessAdmin}

// ErrInvalidDatabaseAccess is what errors.Is matches every refusal of
// database access levels to. The refusals' messages are the chart's own
// (application.postgres.access in chart/application/templates/_helpers.tpl),
// word for word.
var ErrInvalidDatabaseAccess = errors.New("invalid database access")

// accessError is a refusal of database access levels: the chart's message
// alone, which errors.Is matches to ErrInvalidDatabaseAccess.
type accessError struct{ message string }

func (e *accessError) Error() string { return e.message }

func (e *accessError) Is(target error) bool { return target == ErrInvalidDatabaseAccess }

func invalidAccess(format string, args ...any) error {
	return &accessError{message: fmt.Sprintf(format, args...)}
}

// DatabaseAccess is an Environment's postgres.access: the level that
// qualifies for read-write and the level that qualifies for read-only.
type DatabaseAccess struct {
	ReadWrite string
	ReadOnly  string
}

// DefaultDatabaseAccess is the access an Environment has when its values
// file sets none: read-write for push on staging, and so on a Preview
// Environment, which renders from staging's values file; nothing on prod.
func DefaultDatabaseAccess(environment string) DatabaseAccess {
	if environment == "prod" {
		return DatabaseAccess{ReadWrite: AccessNone, ReadOnly: AccessNone}
	}
	return DatabaseAccess{ReadWrite: AccessPush, ReadOnly: AccessNone}
}

// ResolveDatabaseAccess checks the levels explicitly set for an Environment,
// "" meaning unset, and returns them with the Environment's defaults for
// the unset ones. It refuses what the chart refuses, with the chart's
// message: an unknown level, a level other than none without the Postgres
// Capability, and read-only needing more permission than read-write when
// neither is none.
func ResolveDatabaseAccess(environment string, postgres bool, explicit DatabaseAccess) (DatabaseAccess, error) {
	fields := []struct{ key, level string }{{"readWrite", explicit.ReadWrite}, {"readOnly", explicit.ReadOnly}}
	for _, f := range fields {
		if f.level != "" && !slices.Contains(accessLevels, f.level) {
			return DatabaseAccess{}, invalidAccess("postgres.access.%s must be none, pull, push, maintain or admin, got %q", f.key, f.level)
		}
	}
	for _, f := range fields {
		if f.level != "" && f.level != AccessNone && !postgres {
			return DatabaseAccess{}, invalidAccess("postgres.access.%s: %s needs postgres.enabled: true; there is no database to give access to", f.key, f.level)
		}
	}
	access := DefaultDatabaseAccess(environment)
	if explicit.ReadWrite != "" {
		access.ReadWrite = explicit.ReadWrite
	}
	if explicit.ReadOnly != "" {
		access.ReadOnly = explicit.ReadOnly
	}
	if access.ReadWrite != AccessNone && access.ReadOnly != AccessNone && slices.Index(accessLevels, access.ReadOnly) > slices.Index(accessLevels, access.ReadWrite) {
		return DatabaseAccess{}, invalidAccess("postgres.access.readOnly: %s needs more permission than postgres.access.readWrite: %s; whoever may write may also read, so readOnly must need no more permission than readWrite", access.ReadOnly, access.ReadWrite)
	}
	return access, nil
}

// AccessRole is one of the two Postgres roles database access opens.
type AccessRole int

const (
	// ReadWriteRole is <application>_write: it reads and writes every
	// table, and changes no schema.
	ReadWriteRole AccessRole = iota
	// ReadOnlyRole is <application>_read: it reads every table.
	ReadOnlyRole
)

// AccessRoles are both roles, read-write first.
var AccessRoles = []AccessRole{ReadWriteRole, ReadOnlyRole}

// accessRoles describes each AccessRole, indexed by it.
var accessRoles = [...]struct {
	// words is the role's level in words; suffix makes its Postgres name;
	// key is the key under postgres naming its password Secret; slug
	// names that Secret and its file in the Platform repository.
	words, suffix, key, slug string
}{
	ReadWriteRole: {"read-write", "_write", "readWritePasswordSecret", "db-write"},
	ReadOnlyRole:  {"read-only", "_read", "readOnlyPasswordSecret", "db-read"},
}

// String is the role's level in words: read-write or read-only.
func (r AccessRole) String() string { return accessRoles[r].words }

// PostgresRole is the role's name in the Application's database, the
// same in every Environment.
func (r AccessRole) PostgresRole(application string) string {
	return application + accessRoles[r].suffix
}

// PasswordSlug names the role's password Secret and its file in an
// Environment's sops/ directory, the way a secret's KEY slug does.
func (r AccessRole) PasswordSlug() string { return accessRoles[r].slug }

// Level is the role's level in access.
func (r AccessRole) Level(access DatabaseAccess) string {
	if r == ReadOnlyRole {
		return access.ReadOnly
	}
	return access.ReadWrite
}

func (r AccessRole) passwordSecretKey() string { return accessRoles[r].key }

// Database is the Postgres Capability as an Environment's values.yaml
// holds it.
type Database struct {
	Enabled bool
	// Access is the resolved levels, defaults included.
	Access DatabaseAccess
	// ReadWritePasswordSecret and ReadOnlyPasswordSecret name each role's
	// password Secret; "" when the values file names none.
	ReadWritePasswordSecret string
	ReadOnlyPasswordSecret  string
}

// PasswordSecret is the Secret db names for role's password, or "".
func (db Database) PasswordSecret(role AccessRole) string {
	if role == ReadOnlyRole {
		return db.ReadOnlyPasswordSecret
	}
	return db.ReadWritePasswordSecret
}

// ReadDatabase reads the Postgres Capability from an Environment's
// values.yaml, refusing levels the chart would refuse.
func ReadDatabase(valuesYAML []byte) (Database, error) {
	var values struct {
		Environment string `yaml:"environment"`
		Postgres    struct {
			Enabled bool `yaml:"enabled"`
			Access  struct {
				ReadWrite string `yaml:"readWrite"`
				ReadOnly  string `yaml:"readOnly"`
			} `yaml:"access"`
			ReadWritePasswordSecret string `yaml:"readWritePasswordSecret"`
			ReadOnlyPasswordSecret  string `yaml:"readOnlyPasswordSecret"`
		} `yaml:"postgres"`
	}
	if err := yaml.Unmarshal(valuesYAML, &values); err != nil {
		return Database{}, fmt.Errorf("parsing values.yaml: %w", err)
	}
	environment := values.Environment
	if environment == "" {
		// The chart's default.
		environment = "prod"
	}
	access, err := ResolveDatabaseAccess(environment, values.Postgres.Enabled, DatabaseAccess{ReadWrite: values.Postgres.Access.ReadWrite, ReadOnly: values.Postgres.Access.ReadOnly})
	if err != nil {
		return Database{}, err
	}
	return Database{
		Enabled:                 values.Postgres.Enabled,
		Access:                  access,
		ReadWritePasswordSecret: values.Postgres.ReadWritePasswordSecret,
		ReadOnlyPasswordSecret:  values.Postgres.ReadOnlyPasswordSecret,
	}, nil
}

// SetDatabaseAccess sets both of postgres.access's levels in an
// Environment's values.yaml, and reports whether it changed anything.
func SetDatabaseAccess(valuesYAML []byte, access DatabaseAccess) (out []byte, changed bool, err error) {
	root, err := decodeDocument(valuesYAML, "values.yaml")
	if err != nil {
		return nil, false, err
	}
	current := mappingValue(mappingValue(root, "postgres"), "access")
	if v := mappingValue(current, "readWrite"); v != nil && v.Value == access.ReadWrite {
		if v := mappingValue(current, "readOnly"); v != nil && v.Value == access.ReadOnly {
			return valuesYAML, false, nil
		}
	}
	setNestedValue(root, []string{"postgres", "access", "readWrite"}, scalarNode(access.ReadWrite))
	setNestedValue(root, []string{"postgres", "access", "readOnly"}, scalarNode(access.ReadOnly))
	out, err = encodeDocument(root)
	return out, true, err
}

// SetPasswordSecret names the Secret holding role's password in an
// Environment's values.yaml, or with name "" removes the reference, and
// reports whether it changed anything.
func SetPasswordSecret(valuesYAML []byte, role AccessRole, name string) (out []byte, changed bool, err error) {
	root, err := decodeDocument(valuesYAML, "values.yaml")
	if err != nil {
		return nil, false, err
	}
	key := role.passwordSecretKey()
	postgres := mappingValue(root, "postgres")
	current := mappingValue(postgres, key)
	switch {
	case name == "" && current == nil:
		return valuesYAML, false, nil
	case name == "":
		removeMappingKey(postgres, key)
	case current != nil && current.Value == name:
		return valuesYAML, false, nil
	default:
		setNestedValue(root, []string{"postgres", key}, scalarNode(name))
	}
	out, err = encodeDocument(root)
	return out, true, err
}
