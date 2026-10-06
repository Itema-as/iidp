//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testDatabaseAccess proves the roles database access opens on shop-prod,
// whose fixture opens read-write for admin and read-only for pull: as
// shop_write the developer writes a row, as shop_read reads it, and
// neither changes the schema. It logs in over TCP through the database's
// -rw Service with each role's own password, from its Secret, as the
// database tunnel will.
func testDatabaseAccess(ctx context.Context, t *testing.T, cluster *Cluster) {
	t.Helper()
	const namespace, db = "shop-prod", "shop-db"

	// The operator creates the roles after the Cluster is ready, on its own
	// schedule.
	var reconciled []string
	err := pollUntil(ctx, 3*time.Minute, 5*time.Second, func() (bool, error) {
		out, err := cluster.Kubectl(ctx, "-n", namespace, "get", "clusters.postgresql.cnpg.io", db, "-o", "jsonpath={.status.managedRolesStatus.byStatus.reconciled}")
		if err != nil {
			return false, fmt.Errorf("reading %s's managed roles: %w\n%s", db, err, out)
		}
		reconciled = nil
		_ = json.Unmarshal([]byte(out), &reconciled)
		return slices.Contains(reconciled, "shop_read") && slices.Contains(reconciled, "shop_write"), nil
	}, func() error {
		out, _ := cluster.Kubectl(ctx, "-n", namespace, "get", "clusters.postgresql.cnpg.io", db, "-o", "jsonpath={.status.managedRolesStatus}")
		return fmt.Errorf("%s reconciled roles %v, want shop_read and shop_write; managedRolesStatus: %s", db, reconciled, out)
	})
	if err != nil {
		t.Fatal(err)
	}

	// pg_read_all_data and pg_write_all_data, the roles' only grants, came
	// with PostgreSQL 14.
	version, err := psqlAs(ctx, cluster, namespace, db, "shop_read", "SHOW server_version_num")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := strconv.Atoi(version); err != nil || n < 140000 {
		t.Fatalf("server_version_num = %q, want PostgreSQL 14 or later", version)
	}
	t.Logf("CloudNativePG's default image runs PostgreSQL with server_version_num %s", version)

	// The table is the owner's, as a migration would make it.
	if out, err := cluster.Kubectl(ctx, "-n", namespace, "exec", db+"-1", "-c", "postgres", "--",
		"psql", "-d", "shop", "-v", "ON_ERROR_STOP=1", "-c", "SET ROLE shop; CREATE TABLE e2e_access (note text)"); err != nil {
		t.Fatalf("creating a table as the owner: %v\n%s", err, out)
	}

	if _, err := psqlAs(ctx, cluster, namespace, db, "shop_write", "INSERT INTO e2e_access VALUES ('written by shop_write')"); err != nil {
		t.Errorf("shop_write cannot write a row: %v", err)
	}
	if got, err := psqlAs(ctx, cluster, namespace, db, "shop_read", "SELECT note FROM e2e_access"); err != nil || got != "written by shop_write" {
		t.Errorf("shop_read reads %q, %v; want the row shop_write wrote", got, err)
	}
	for _, refused := range []struct{ role, sql string }{
		{"shop_read", "INSERT INTO e2e_access VALUES ('written by shop_read')"},
		{"shop_read", "CREATE TABLE e2e_by_read (note text)"},
		{"shop_write", "CREATE TABLE e2e_by_write (note text)"},
	} {
		out, err := psqlAs(ctx, cluster, namespace, db, refused.role, refused.sql)
		if err == nil || !strings.Contains(out, "permission denied") {
			t.Errorf("%s ran %q: %v %q, want permission denied", refused.role, refused.sql, err, out)
		}
	}

	// The database tunnel may read exactly the two password Secrets.
	for secret, want := range map[string]string{"shop-db-write": "yes", "shop-db-read": "yes", "shop-db-app": "no", "backups-credentials": "no"} {
		out, _ := cluster.Kubectl(ctx, "auth", "can-i", "get", "secret/"+secret, "-n", namespace, "--as", "system:serviceaccount:iidp-db-tunnel:iidp-db-tunnel")
		if got := strings.TrimSpace(out); got != want {
			t.Errorf("the database tunnel may get %s: %q, want %q", secret, got, want)
		}
	}
}

// psqlAs runs one SQL statement on the Application's database as role,
// logging in over TCP through the -rw Service with the password in the
// role's Secret. It returns psql's unaligned output, or its error output
// with the error.
func psqlAs(ctx context.Context, cluster *Cluster, namespace, db, role, sql string) (string, error) {
	secret := map[string]string{"shop_write": "shop-db-write", "shop_read": "shop-db-read"}[role]
	encoded, err := cluster.Kubectl(ctx, "-n", namespace, "get", "secret", secret, "-o", "jsonpath={.data.password}")
	if err != nil {
		return "", fmt.Errorf("reading %s: %w\n%s", secret, err, encoded)
	}
	password, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", fmt.Errorf("decoding %s's password: %w", secret, err)
	}
	out, err := cluster.Kubectl(ctx, "-n", namespace, "exec", db+"-1", "-c", "postgres", "--",
		"env", "PGPASSWORD="+string(password), "psql", "-h", db+"-rw", "-U", role, "-d", "shop",
		"-v", "ON_ERROR_STOP=1", "-At", "-c", sql)
	if err != nil {
		return out, fmt.Errorf("psql as %s: %w\n%s", role, err, out)
	}
	return strings.TrimSpace(out), nil
}
