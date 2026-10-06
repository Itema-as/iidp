package render_test

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/render"
)

func TestDatabaseAccessDefaultsFollowTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		environment string
		explicit    render.DatabaseAccess
		want        render.DatabaseAccess
	}{
		{"prod", render.DatabaseAccess{}, render.DatabaseAccess{ReadWrite: "none", ReadOnly: "none"}},
		{"staging", render.DatabaseAccess{}, render.DatabaseAccess{ReadWrite: "push", ReadOnly: "none"}},
		// A Preview Environment renders from staging's values, so it has
		// staging's defaults.
		{"pr-7", render.DatabaseAccess{}, render.DatabaseAccess{ReadWrite: "push", ReadOnly: "none"}},
		{"staging", render.DatabaseAccess{ReadOnly: "pull"}, render.DatabaseAccess{ReadWrite: "push", ReadOnly: "pull"}},
		{"prod", render.DatabaseAccess{ReadOnly: "maintain"}, render.DatabaseAccess{ReadWrite: "none", ReadOnly: "maintain"}},
		{"prod", render.DatabaseAccess{ReadWrite: "admin", ReadOnly: "admin"}, render.DatabaseAccess{ReadWrite: "admin", ReadOnly: "admin"}},
	} {
		got, err := render.ResolveDatabaseAccess(tc.environment, true, tc.explicit)
		if err != nil {
			t.Errorf("%s %+v: %v", tc.environment, tc.explicit, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s %+v resolves to %+v, want %+v", tc.environment, tc.explicit, got, tc.want)
		}
	}
}

func TestDatabaseAccessRefusalsWrapErrInvalidDatabaseAccess(t *testing.T) {
	for _, tc := range []struct {
		environment string
		postgres    bool
		explicit    render.DatabaseAccess
	}{
		{"staging", true, render.DatabaseAccess{ReadOnly: "read"}},
		// staging's default read-write, push, needs less than admin.
		{"staging", true, render.DatabaseAccess{ReadOnly: "admin"}},
		{"staging", false, render.DatabaseAccess{ReadWrite: "push"}},
	} {
		_, err := render.ResolveDatabaseAccess(tc.environment, tc.postgres, tc.explicit)
		if !errors.Is(err, render.ErrInvalidDatabaseAccess) {
			t.Errorf("%s postgres=%v %+v: err = %v, want ErrInvalidDatabaseAccess", tc.environment, tc.postgres, tc.explicit, err)
		}
	}
	// Without Postgres, the defaults and an explicit none open nothing, so
	// there is nothing to refuse.
	for _, explicit := range []render.DatabaseAccess{{}, {ReadWrite: "none", ReadOnly: "none"}} {
		if _, err := render.ResolveDatabaseAccess("staging", false, explicit); err != nil {
			t.Errorf("staging without Postgres, %+v: %v", explicit, err)
		}
	}
}

const testPostgresValuesYAML = `# Values for the staging Environment of shop.
application:
    name: shop
environment: staging
postgres:
    # A comment the edits keep.
    enabled: true
    migrationCommand: ""
    backupRetention: 30d
`

func TestReadDatabaseResolvesTheLevelsAndNamesThePasswords(t *testing.T) {
	db, err := render.ReadDatabase([]byte(testPostgresValuesYAML))
	if err != nil {
		t.Fatal(err)
	}
	want := render.Database{Enabled: true, Access: render.DatabaseAccess{ReadWrite: "push", ReadOnly: "none"}}
	if db != want {
		t.Errorf("ReadDatabase = %+v, want %+v", db, want)
	}

	values, _, err := render.SetDatabaseAccess([]byte(testPostgresValuesYAML), render.DatabaseAccess{ReadWrite: "admin", ReadOnly: "pull"})
	if err != nil {
		t.Fatal(err)
	}
	if values, _, err = render.SetPasswordSecret(values, render.ReadOnlyRole, "shop-staging-db-read"); err != nil {
		t.Fatal(err)
	}
	db, err = render.ReadDatabase(values)
	if err != nil {
		t.Fatal(err)
	}
	want = render.Database{Enabled: true, Access: render.DatabaseAccess{ReadWrite: "admin", ReadOnly: "pull"}, ReadOnlyPasswordSecret: "shop-staging-db-read"}
	if db != want {
		t.Errorf("ReadDatabase = %+v, want %+v", db, want)
	}
	if got := db.PasswordSecret(render.ReadOnlyRole); got != "shop-staging-db-read" {
		t.Errorf("PasswordSecret(ReadOnlyRole) = %q", got)
	}
}

func TestReadDatabaseRefusesWhatTheChartRefuses(t *testing.T) {
	values := strings.Replace(testPostgresValuesYAML, "    enabled: true\n", "    enabled: true\n    access:\n        readWrite: pull\n        readOnly: admin\n", 1)
	if _, err := render.ReadDatabase([]byte(values)); !errors.Is(err, render.ErrInvalidDatabaseAccess) {
		t.Errorf("err = %v, want ErrInvalidDatabaseAccess", err)
	}
}

func TestSetDatabaseAccessWritesBothLevelsInPlace(t *testing.T) {
	out, changed, err := render.SetDatabaseAccess([]byte(testPostgresValuesYAML), render.DatabaseAccess{ReadWrite: "push", ReadOnly: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("changed = false, want true: the file had no access block")
	}
	for _, want := range []string{"# Values for the staging Environment of shop.", "# A comment the edits keep.", "    access:\n        readWrite: push\n        readOnly: none\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the values lack %q:\n%s", want, out)
		}
	}
	again, changed, err := render.SetDatabaseAccess(out, render.DatabaseAccess{ReadWrite: "push", ReadOnly: "none"})
	if err != nil || changed || string(again) != string(out) {
		t.Errorf("setting the same levels again: changed = %v, err = %v", changed, err)
	}
}

func TestSetPasswordSecretNamesAndRemovesIt(t *testing.T) {
	named, changed, err := render.SetPasswordSecret([]byte(testPostgresValuesYAML), render.ReadWriteRole, "shop-staging-db-write")
	if err != nil || !changed {
		t.Fatalf("naming it: changed = %v, err = %v", changed, err)
	}
	if !strings.Contains(string(named), "    readWritePasswordSecret: shop-staging-db-write\n") {
		t.Errorf("no readWritePasswordSecret:\n%s", named)
	}
	removed, changed, err := render.SetPasswordSecret(named, render.ReadWriteRole, "")
	if err != nil || !changed {
		t.Fatalf("removing it: changed = %v, err = %v", changed, err)
	}
	if strings.Contains(string(removed), "PasswordSecret") {
		t.Errorf("the reference is still there:\n%s", removed)
	}
	if _, changed, _ := render.SetPasswordSecret(removed, render.ReadWriteRole, ""); changed {
		t.Error("removing an absent reference changed the file")
	}
}

func TestTheRolesAreNamedAfterTheApplication(t *testing.T) {
	if got := render.ReadWriteRole.PostgresRole("shop"); got != "shop_write" {
		t.Errorf("ReadWriteRole = %q, want shop_write", got)
	}
	if got := render.ReadOnlyRole.PostgresRole("my-shop"); got != "my-shop_read" {
		t.Errorf("ReadOnlyRole = %q, want my-shop_read", got)
	}
}

func TestPasswordSecretDocumentIsBasicAuthForTheRole(t *testing.T) {
	doc, err := render.PasswordSecretDocument(render.PasswordSecret{
		Name: "shop-staging-db-write", Application: "shop", Environment: "staging", Username: "shop_write", Password: "s3cret",
	})
	if err != nil {
		t.Fatal(err)
	}
	var secret struct {
		Kind     string `yaml:"kind"`
		Type     string `yaml:"type"`
		Metadata struct {
			Name        string            `yaml:"name"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
		StringData map[string]string `yaml:"stringData"`
	}
	if err := yaml.Unmarshal(doc, &secret); err != nil {
		t.Fatal(err)
	}
	if secret.Kind != "Secret" || secret.Type != "kubernetes.io/basic-auth" || secret.Metadata.Name != "shop-staging-db-write" {
		t.Errorf("Secret = %+v, want kubernetes.io/basic-auth shop-staging-db-write", secret)
	}
	if len(secret.StringData) != 2 || secret.StringData["username"] != "shop_write" || secret.StringData["password"] != "s3cret" {
		t.Errorf("stringData = %v, want exactly username and password", secret.StringData)
	}
	// CloudNativePG applies a password change at once only in a Secret
	// labelled to reload.
	if secret.Metadata.Labels["cnpg.io/reload"] != "true" || secret.Metadata.Labels["iidp.itema.no/environment"] != "staging" {
		t.Errorf("labels = %v", secret.Metadata.Labels)
	}
	if secret.Metadata.Annotations["kustomize.config.k8s.io/needs-hash"] != "false" || secret.Metadata.Annotations["argocd.argoproj.io/sync-wave"] != "-2" {
		t.Errorf("annotations = %v", secret.Metadata.Annotations)
	}
}

// prod's levels and passwords are prod's: a new staging starts at its own
// defaults, and the CLI writes its passwords.
func TestCopyValuesForStagingDropsProdsDatabaseAccess(t *testing.T) {
	prod := strings.Replace(testPostgresValuesYAML, "environment: staging", "environment: prod", 1)
	values, _, err := render.SetDatabaseAccess([]byte(prod), render.DatabaseAccess{ReadWrite: "none", ReadOnly: "maintain"})
	if err != nil {
		t.Fatal(err)
	}
	if values, _, err = render.SetPasswordSecret(values, render.ReadOnlyRole, "shop-db-read"); err != nil {
		t.Fatal(err)
	}
	out, _, err := render.CopyValuesForStaging(values)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "access") || strings.Contains(string(out), "PasswordSecret") {
		t.Errorf("staging kept prod's database access:\n%s", out)
	}
	db, err := render.ReadDatabase(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := (render.Database{Enabled: true, Access: render.DefaultDatabaseAccess("staging")}); db != want {
		t.Errorf("staging's database = %+v, want %+v", db, want)
	}
}
