//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Itema-as/iidp/internal/render"
)

// testDatabaseAccess proves the roles database access opens on shop-prod,
// whose fixture opens read-write for admin and read-only for pull: as
// shop_write the developer writes a row, as shop_read reads it, and
// neither changes the schema. Closing read-only then drops shop_read. It logs in over TCP through the database's
// -rw Service with each role's own password, from its Secret, as the
// Database tunnel will.
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

	// The Database tunnel may read exactly the two password Secrets.
	checkTunnelSecrets(ctx, t, cluster, namespace, map[string]string{"shop-db-write": "yes", "shop-db-read": "yes", "shop-db-app": "no", "backups-credentials": "no"})

	// Closing read-only the way iidp app db access --read-only none does
	// drops shop_read from the database and takes the tunnel's access to
	// its password away.
	err = cluster.PushToRepository(ctx, "iidp-platform", "test: iidp app db access shop --env prod --read-only none", func(dir string) (bool, error) {
		prod := filepath.Join(dir, "applications", "shop", "prod")
		values, err := os.ReadFile(filepath.Join(prod, "values.yaml"))
		if err != nil {
			return false, err
		}
		values, _, err = render.SetDatabaseAccess(values, render.DatabaseAccess{ReadWrite: "admin", ReadOnly: "none"})
		if err != nil {
			return false, err
		}
		if values, _, err = render.SetPasswordSecret(values, render.ReadOnlyRole, ""); err != nil {
			return false, err
		}
		if err := os.WriteFile(filepath.Join(prod, "values.yaml"), values, 0o644); err != nil {
			return false, err
		}
		if err := os.Remove(filepath.Join(prod, "sops", "db-read.enc.yaml")); err != nil {
			return false, err
		}
		return true, os.WriteFile(filepath.Join(prod, "sops", "ksops.yaml"), render.KsopsGenerator("shop-secrets", []string{"backups-credentials.enc.yaml", "db-write.enc.yaml"}), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.RefreshApplication(ctx, "shop-prod"); err != nil {
		t.Fatal(err)
	}
	var count string
	err = pollUntil(ctx, 3*time.Minute, 5*time.Second, func() (bool, error) {
		out, err := cluster.Kubectl(ctx, "-n", namespace, "exec", db+"-1", "-c", "postgres", "--",
			"psql", "-d", "shop", "-At", "-c", "SELECT count(*) FROM pg_roles WHERE rolname = 'shop_read'")
		if err != nil {
			return false, fmt.Errorf("counting shop_read: %w\n%s", err, out)
		}
		count = strings.TrimSpace(out)
		return count == "0", nil
	}, func() error {
		out, _ := cluster.Kubectl(ctx, "-n", namespace, "get", "clusters.postgresql.cnpg.io", db, "-o", "jsonpath={.spec.managed.roles} {.status.managedRolesStatus}")
		return fmt.Errorf("shop_read still exists (%s) after its level was closed; the Cluster: %s", count, out)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := psqlAs(ctx, cluster, namespace, db, "shop_write", "SELECT count(*) FROM e2e_access"); err != nil {
		t.Errorf("shop_write cannot read after read-only closed: %v", err)
	}
	// The Role is applied in a later sync wave than the Cluster, after the
	// migration Job, so it can still name shop-db-read when shop_read is
	// already gone.
	err = pollUntil(ctx, 3*time.Minute, 5*time.Second, func() (bool, error) {
		return tunnelMayGet(ctx, cluster, namespace, "shop-db-read") == "no", nil
	}, func() error {
		return errors.New("the Database tunnel may still get shop-db-read after read-only was closed")
	})
	if err != nil {
		t.Fatal(err)
	}
	checkTunnelSecrets(ctx, t, cluster, namespace, map[string]string{"shop-db-write": "yes", "shop-db-read": "no"})
}

// checkTunnelSecrets checks which Secrets in namespace the database
// tunnel's ServiceAccount may get, "yes" or "no" for each.
func checkTunnelSecrets(ctx context.Context, t *testing.T, cluster *Cluster, namespace string, want map[string]string) {
	t.Helper()
	for secret, want := range want {
		if got := tunnelMayGet(ctx, cluster, namespace, secret); got != want {
			t.Errorf("the Database tunnel may get %s: %q, want %q", secret, got, want)
		}
	}
}

// tunnelMayGet is kubectl auth can-i's answer, "yes" or "no", to whether
// the Database tunnel's ServiceAccount may get secret in namespace.
func tunnelMayGet(ctx context.Context, cluster *Cluster, namespace, secret string) string {
	out, _ := cluster.Kubectl(ctx, "auth", "can-i", "get", "secret/"+secret, "-n", namespace, "--as", "system:serviceaccount:iidp-db-tunnel:iidp-db-tunnel")
	return strings.TrimSpace(out)
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
