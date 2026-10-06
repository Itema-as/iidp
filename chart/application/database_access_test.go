package application_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	valuesrender "github.com/Itema-as/iidp/internal/render"
)

// Database access: postgres.access's levels, the <app>_read and <app>_write
// roles they open, and what the database tunnel is granted to log in as
// them.

const (
	readWriteAnnotation = "iidp.itema.no/db-access-read-write"
	readOnlyAnnotation  = "iidp.itema.no/db-access-read-only"
)

// managedRoles returns the Cluster's spec.managed.roles, keyed by name.
func managedRoles(t *testing.T, cluster object) map[string]map[string]any {
	t.Helper()
	roles := map[string]map[string]any{}
	managed, ok := get[map[string]any](t, cluster, "spec")["managed"].(map[string]any)
	if !ok {
		return roles
	}
	for _, r := range get[[]any](t, managed, "roles") {
		role := r.(map[string]any)
		roles[get[string](t, role, "name")] = role
	}
	return roles
}

// tunnelRole returns the Environment's Role and RoleBinding for the
// database tunnel, or nils when neither renders. It fails when only one
// does.
func tunnelRole(t *testing.T, objects map[string]object, fullname string) (role, binding object) {
	t.Helper()
	role, roleOK := objects["Role/"+fullname+"-db-tunnel"]
	binding, bindingOK := objects["RoleBinding/"+fullname+"-db-tunnel"]
	if roleOK != bindingOK {
		t.Fatalf("rendered Role %v and RoleBinding %v for the database tunnel; want both or neither", roleOK, bindingOK)
	}
	return role, binding
}

// tunnelSecrets returns the Secrets the database tunnel's Role lets it get,
// checking that it grants nothing else.
func tunnelSecrets(t *testing.T, role object) []string {
	t.Helper()
	rules := get[[]any](t, role, "rules")
	if len(rules) != 1 {
		t.Fatalf("the database tunnel's Role has %d rules, want 1: %v", len(rules), rules)
	}
	rule := rules[0]
	if groups := anyStrings(get[[]any](t, rule, "apiGroups")); !slices.Equal(groups, []string{""}) {
		t.Errorf("apiGroups = %v, want the core group only", groups)
	}
	if resources := anyStrings(get[[]any](t, rule, "resources")); !slices.Equal(resources, []string{"secrets"}) {
		t.Errorf("resources = %v, want secrets only", resources)
	}
	if verbs := anyStrings(get[[]any](t, rule, "verbs")); !slices.Equal(verbs, []string{"get"}) {
		t.Errorf("verbs = %v, want get only", verbs)
	}
	names := anyStrings(get[[]any](t, rule, "resourceNames"))
	slices.Sort(names)
	return names
}

func TestDatabaseAccessOpensOnlyTheRolesWithALevelAndANamedPassword(t *testing.T) {
	for _, tc := range []struct {
		fixture, fullname   string
		readWrite, readOnly string
		// roles maps each rendered role to its password Secret.
		roles map[string]string
	}{
		// staging defaults to read-write for push and read-only for none, so
		// the named read-only password opens nothing.
		{"db-access-staging-default.yaml", "shop-staging", "push", "none", map[string]string{"shop_write": "shop-staging-db-write"}},
		// prod defaults to closed.
		{"postgres-prod.yaml", "shop", "none", "none", map[string]string{}},
		{"db-access-prod-read-only.yaml", "shop", "none", "maintain", map[string]string{"shop_read": "shop-db-read"}},
		// Levels without passwords: not set up yet, so effectively closed.
		{"db-access-password-not-named.yaml", "shop-staging", "push", "pull", map[string]string{}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			objects := render(t, tc.fixture)
			cluster := mustObject(t, objects, "Cluster/"+tc.fullname+"-db")

			annotations := annotationsOf(t, cluster)
			if got := annotations[readWriteAnnotation]; got != tc.readWrite {
				t.Errorf("%s = %v, want %q", readWriteAnnotation, got, tc.readWrite)
			}
			if got := annotations[readOnlyAnnotation]; got != tc.readOnly {
				t.Errorf("%s = %v, want %q", readOnlyAnnotation, got, tc.readOnly)
			}

			roles := map[string]string{}
			for name, role := range managedRoles(t, cluster) {
				roles[name] = get[string](t, role, "passwordSecret", "name")
			}
			if !mapsEqual(roles, tc.roles) {
				t.Errorf("managed roles and their password Secrets = %v, want %v", roles, tc.roles)
			}

			role, binding := tunnelRole(t, objects, tc.fullname)
			if len(tc.roles) == 0 {
				if role != nil {
					t.Errorf("rendered the database tunnel's Role with no role to log in as: %v", role)
				}
				return
			}
			if role == nil {
				t.Fatalf("no Role/%s-db-tunnel rendered; got %v", tc.fullname, keys(objects))
			}
			var want []string
			for _, secret := range tc.roles {
				want = append(want, secret)
			}
			slices.Sort(want)
			if got := tunnelSecrets(t, role); !slices.Equal(got, want) {
				t.Errorf("the database tunnel may get %v, want exactly %v", got, want)
			}
			if ref := get[map[string]any](t, binding, "roleRef"); ref["kind"] != "Role" || ref["name"] != tc.fullname+"-db-tunnel" || ref["apiGroup"] != "rbac.authorization.k8s.io" {
				t.Errorf("roleRef = %v, want Role %s-db-tunnel", ref, tc.fullname)
			}
			subjects := get[[]any](t, binding, "subjects")
			want0 := map[string]any{"kind": "ServiceAccount", "name": "iidp-db-tunnel", "namespace": "iidp-db-tunnel"}
			if len(subjects) != 1 || !mapsEqual(subjects[0].(map[string]any), want0) {
				t.Errorf("subjects = %v, want only %v", subjects, want0)
			}
		})
	}
}

func TestTheDatabaseTunnelsServiceAccountIsAPlatformValue(t *testing.T) {
	objects := render(t, "db-access-staging-default.yaml",
		"--set", "platform.databaseTunnel.serviceAccount=tunnel", "--set", "platform.databaseTunnel.namespace=platform-db")
	_, binding := tunnelRole(t, objects, "shop-staging")
	subjects := get[[]any](t, binding, "subjects")
	if len(subjects) != 1 || subjects[0].(map[string]any)["name"] != "tunnel" || subjects[0].(map[string]any)["namespace"] != "platform-db" {
		t.Errorf("subjects = %v, want ServiceAccount tunnel in platform-db", subjects)
	}
}

// The roles read and write data and nothing else: no DDL, since schema
// changes go through migrations, and no way to raise their own privileges.
func TestTheAccessRolesAreMembersOfThePredefinedRolesOnly(t *testing.T) {
	wantInRoles := map[string][]string{
		"shop_read":  {"pg_read_all_data"},
		"shop_write": {"pg_read_all_data", "pg_write_all_data"},
	}
	seen := map[string]bool{}
	for fixture, cluster := range map[string]string{"db-access-staging-default.yaml": "shop-staging-db", "db-access-prod-read-only.yaml": "shop-db"} {
		cluster := mustObject(t, render(t, fixture), "Cluster/"+cluster)
		if enabled, set := get[map[string]any](t, cluster, "spec")["enableSuperuserAccess"]; set && enabled != false {
			t.Errorf("%s: enableSuperuserAccess = %v, want the postgres superuser disabled", fixture, enabled)
		}
		for name, role := range managedRoles(t, cluster) {
			seen[name] = true
			if got := anyStrings(get[[]any](t, role, "inRoles")); !slices.Equal(got, wantInRoles[name]) {
				t.Errorf("%s: %s inRoles = %v, want exactly %v", fixture, name, got, wantInRoles[name])
			}
			if limit := get[int](t, role, "connectionLimit"); limit != 3 {
				t.Errorf("%s: %s connectionLimit = %d, want 3", fixture, name, limit)
			}
			if !get[bool](t, role, "login") {
				t.Errorf("%s: %s cannot log in", fixture, name)
			}
			if !get[bool](t, role, "inherit") {
				t.Errorf("%s: %s does not inherit its predefined roles' privileges", fixture, name)
			}
			if ensure := get[string](t, role, "ensure"); ensure != "present" {
				t.Errorf("%s: %s ensure = %q, want present", fixture, name, ensure)
			}
			for _, attribute := range []string{"superuser", "createdb", "createrole", "bypassrls", "replication"} {
				if v, set := role[attribute]; set && v != false {
					t.Errorf("%s: %s %s = %v, want false", fixture, name, attribute, v)
				}
			}
		}
	}
	if !seen["shop_read"] || !seen["shop_write"] {
		t.Fatalf("checked %v, want both shop_read and shop_write", seen)
	}
}

// databaseAccessRefusals are the values the chart and the CLI both refuse,
// with the message both give.
var databaseAccessRefusals = []struct{ fixture, message string }{
	{"refuse-db-access-unknown-level.yaml", `postgres.access.readWrite must be none, pull, push, maintain or admin, got "triage"`},
	{"refuse-db-access-read-only-stricter.yaml", `postgres.access.readOnly: admin needs more permission than postgres.access.readWrite: pull; whoever may write may also read, so readOnly must need no more permission than readWrite`},
	{"refuse-db-access-without-postgres.yaml", `postgres.access.readOnly: pull needs postgres.enabled: true; there is no database to give access to`},
}

func TestRenderingRefusesDatabaseAccessItCannotHonour(t *testing.T) {
	for _, tc := range databaseAccessRefusals {
		t.Run(tc.fixture, func(t *testing.T) {
			out, err := helmTemplate(t, tc.fixture)
			if err == nil {
				t.Fatalf("rendering succeeded, want a failure:\n%s", out)
			}
			if !strings.Contains(out, tc.message) {
				t.Fatalf("error does not say %q:\n%s", tc.message, out)
			}
		})
	}
}

// The CLI checks the same values with internal/render before it writes
// them, and must say exactly what the chart says.
func TestTheCLIRefusesTheSameDatabaseAccessWithTheSameMessage(t *testing.T) {
	for _, tc := range databaseAccessRefusals {
		t.Run(tc.fixture, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			var values struct {
				Environment string `yaml:"environment"`
				Postgres    struct {
					Enabled bool `yaml:"enabled"`
					Access  struct {
						ReadWrite string `yaml:"readWrite"`
						ReadOnly  string `yaml:"readOnly"`
					} `yaml:"access"`
				} `yaml:"postgres"`
			}
			if err := yaml.Unmarshal(data, &values); err != nil {
				t.Fatal(err)
			}
			_, err = valuesrender.ResolveDatabaseAccess(values.Environment, values.Postgres.Enabled,
				valuesrender.DatabaseAccess{ReadWrite: values.Postgres.Access.ReadWrite, ReadOnly: values.Postgres.Access.ReadOnly})
			if err == nil || err.Error() != tc.message {
				t.Fatalf("the CLI's check says %v, want %q", err, tc.message)
			}
		})
	}
}

// mapsEqual compares two maps of comparable values.
func mapsEqual[K comparable, V comparable](a, b map[K]V) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
