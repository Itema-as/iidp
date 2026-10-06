//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// testDatabaseTunnel proves iidp app db connect reaches shop's databases
// through the database tunnel, Traefik and a WebSocket, logged in as the
// role the developer's permission gives them (push, in the fake GitHub):
//   - on staging, read-write for push, it connects as shop_write and
//     inserts a row, and the session outlives Traefik's 60-second read
//     timeout;
//   - with --read-only it connects as shop_read, which reads the row and
//     cannot insert one;
//   - prod, which testDatabaseAccess left read-write for admin and
//     read-only for nobody, refuses it with the levels and the command
//     that changes them;
//   - each session's start and end, and the refusal, are Events on the
//     Environment's ArgoCD Application.
//
// It runs psql on the host, as a developer would.
func testDatabaseTunnel(ctx context.Context, t *testing.T, cluster *Cluster) {
	t.Helper()
	if _, err := exec.LookPath("psql"); err != nil {
		t.Fatal("the database tunnel's test needs psql on PATH, as a developer's machine has it")
	}
	if err := cluster.WaitForDatabaseTunnel(ctx, 3*time.Minute); err != nil {
		t.Fatal(err)
	}
	waitForRoles(ctx, t, cluster, "shop-staging", "shop-staging-db", "shop_read", "shop_write")
	if out, err := cluster.Kubectl(ctx, "-n", "shop-staging", "exec", "shop-staging-db-1", "-c", "postgres", "--",
		"psql", "-d", "shop", "-v", "ON_ERROR_STOP=1", "-c", "SET ROLE shop; CREATE TABLE e2e_tunnel (note text)"); err != nil {
		t.Fatalf("creating a table as the owner: %v\n%s", err, out)
	}

	err := cluster.withGitServerPortForward(ctx, "iidp-platform", func(ctx context.Context, platformRepo string) error {
		// No --env: staging, since shop has one.
		conn, err := cluster.DatabaseConnect(ctx, platformRepo, "shop")
		if err != nil {
			return err
		}
		if conn.ConnString == "" || !strings.HasPrefix(conn.ConnString, "postgresql://shop_write@127.0.0.1:") {
			code, stdout, stderr := conn.Stop()
			return fmt.Errorf("iidp app db connect shop: exit %d, connection string %q\nstdout:\n%s\nstderr:\n%s", code, conn.ConnString, stdout, stderr)
		}
		got, err := psqlThrough(ctx, conn.ConnString, "INSERT INTO e2e_tunnel VALUES ('through the tunnel');\n"+
			"SELECT current_user;\n"+
			// Idle past Traefik's 60-second readTimeout, which a
			// hijacked connection no longer has.
			"\\! sleep 65\n"+
			"SELECT 'still connected as ' || current_user;\n")
		if err != nil {
			t.Errorf("psql through the tunnel to staging: %v", err)
		} else if got != "INSERT 0 1\nshop_write\nstill connected as shop_write" {
			t.Errorf("psql through the tunnel to staging printed %q, want the insert, then shop_write before and after a minute's idle", got)
		}
		if code, _, stderr := conn.Stop(); code != 0 {
			t.Errorf("iidp app db connect shop exited %d after Ctrl-C: %s", code, stderr)
		}

		conn, err = cluster.DatabaseConnect(ctx, platformRepo, "shop", "--env", "staging", "--read-only")
		if err != nil {
			return err
		}
		if !strings.HasPrefix(conn.ConnString, "postgresql://shop_read@127.0.0.1:") {
			code, stdout, stderr := conn.Stop()
			return fmt.Errorf("iidp app db connect shop --read-only: exit %d, connection string %q\nstdout:\n%s\nstderr:\n%s", code, conn.ConnString, stdout, stderr)
		}
		if got, err := psqlThrough(ctx, conn.ConnString, "SELECT current_user || ' reads ' || note FROM e2e_tunnel;\n"); err != nil || got != "shop_read reads through the tunnel" {
			t.Errorf("--read-only reads %q, %v; want shop_read reading the row", got, err)
		}
		if got, err := psqlThrough(ctx, conn.ConnString, "INSERT INTO e2e_tunnel VALUES ('by shop_read');\n"); err == nil || !strings.Contains(got, "permission denied") {
			t.Errorf("--read-only inserted: %q, %v; want permission denied", got, err)
		}
		conn.Stop()

		conn, err = cluster.DatabaseConnect(ctx, platformRepo, "shop", "--env", "prod")
		if err != nil {
			return err
		}
		code, stdout, stderr := conn.Stop()
		want := "refused: your permission on Itema-as/shop is push, and shop prod's database admits read-write for admin and read-only for nobody (none). iidp app db access shop --env prod changes who may connect"
		if code == 0 || conn.ConnString != "" || !strings.Contains(stderr, want) {
			t.Errorf("iidp app db connect shop --env prod: exit %d\nstdout:\n%s\nstderr:\n%s\nwant it refused with %q", code, stdout, stderr, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	events := tunnelEvents(ctx, t, cluster, 5)
	var seen []string
	for _, e := range events {
		seen = append(seen, e.Reason+" "+e.Regarding.Name+" "+e.Metadata.Annotations["iidp.itema.no/role"])
		if e.Metadata.Annotations["iidp.itema.no/login"] != DeveloperLogin {
			t.Errorf("an Event names %q, want the developer's login %s: %+v", e.Metadata.Annotations["iidp.itema.no/login"], DeveloperLogin, e)
		}
	}
	for _, want := range []string{
		"DatabaseSessionStarted shop-staging shop_write",
		"DatabaseSessionEnded shop-staging shop_write",
		"DatabaseSessionStarted shop-staging shop_read",
		"DatabaseSessionEnded shop-staging shop_read",
		"DatabaseSessionRefused shop-prod ",
	} {
		if !slices.Contains(seen, want) {
			t.Errorf("the tunnel's Events are %v, want one %q", seen, want)
		}
	}
	for _, e := range events {
		if e.Reason == "DatabaseSessionEnded" && e.Metadata.Annotations["iidp.itema.no/bytes-to-client"] == "0" {
			t.Errorf("a session's end counts no bytes to the client: %+v", e)
		}
	}
	logs, err := cluster.Kubectl(ctx, "-n", "iidp-db-tunnel", "logs", "deployment/iidp-db-tunnel")
	if err != nil {
		t.Fatalf("reading the tunnel's log: %v\n%s", err, logs)
	}
	for _, never := range []string{"through the tunnel", "INSERT", DeveloperToken} {
		if strings.Contains(logs, never) {
			t.Errorf("the tunnel's log holds %q:\n%s", never, logs)
		}
	}
}

// testPreviewThroughTheTunnel proves a Preview Environment's database is
// reached with staging's levels, as staging's notes_write role.
func testPreviewThroughTheTunnel(ctx context.Context, t *testing.T, cluster *Cluster, application, number string) {
	t.Helper()
	namespace := application + "-pr-" + number
	waitForRoles(ctx, t, cluster, namespace, namespace+"-db", application+"_write")
	err := cluster.withGitServerPortForward(ctx, "iidp-platform", func(ctx context.Context, platformRepo string) error {
		conn, err := cluster.DatabaseConnect(ctx, platformRepo, application, "--pr", number)
		if err != nil {
			return err
		}
		defer conn.Stop()
		if !strings.HasPrefix(conn.ConnString, "postgresql://"+application+"_write@127.0.0.1:") {
			code, stdout, stderr := conn.Stop()
			return fmt.Errorf("iidp app db connect %s --pr %s: exit %d, connection string %q\nstdout:\n%s\nstderr:\n%s", application, number, code, conn.ConnString, stdout, stderr)
		}
		if got, err := psqlThrough(ctx, conn.ConnString, "SELECT current_user || ' on ' || current_database();\n"); err != nil || got != application+"_write on "+application {
			t.Errorf("psql through the tunnel to the preview printed %q, %v", got, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// waitForRoles waits for CloudNativePG to have reconciled roles on the
// Cluster db in namespace.
func waitForRoles(ctx context.Context, t *testing.T, cluster *Cluster, namespace, db string, roles ...string) {
	t.Helper()
	var reconciled []string
	err := pollUntil(ctx, 3*time.Minute, 5*time.Second, func() (bool, error) {
		out, err := cluster.Kubectl(ctx, "-n", namespace, "get", "clusters.postgresql.cnpg.io", db, "-o", "jsonpath={.status.managedRolesStatus.byStatus.reconciled}")
		if err != nil {
			return false, nil
		}
		reconciled = nil
		_ = json.Unmarshal([]byte(out), &reconciled)
		for _, role := range roles {
			if !slices.Contains(reconciled, role) {
				return false, nil
			}
		}
		return true, nil
	}, func() error {
		out, _ := cluster.Kubectl(ctx, "-n", namespace, "get", "clusters.postgresql.cnpg.io", db, "-o", "jsonpath={.status.managedRolesStatus}")
		return fmt.Errorf("%s reconciled roles %v, want %v; managedRolesStatus: %s", db, reconciled, roles, out)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// psqlThrough runs script with the host's psql against the connection
// string iidp app db connect printed, and returns its unaligned output,
// or its error output with the error.
func psqlThrough(ctx context.Context, conninfo, script string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "psql", conninfo, "-X", "-v", "ON_ERROR_STOP=1", "-At", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	// Nothing of the developer's own environment may point psql
	// elsewhere, or answer for the tunnel with a password.
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "PG") {
			env = append(env, e)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("psql: %w\n%s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

type tunnelEvent struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Reason              string `json:"reason"`
	ReportingController string `json:"reportingController"`
	Regarding           struct {
		Name string `json:"name"`
	} `json:"regarding"`
	Note string `json:"note"`
}

// tunnelEvents waits for at least n of the database tunnel's Events in
// argocd and returns them all.
func tunnelEvents(ctx context.Context, t *testing.T, cluster *Cluster, n int) []tunnelEvent {
	t.Helper()
	var events []tunnelEvent
	err := pollUntil(ctx, 30*time.Second, 2*time.Second, func() (bool, error) {
		out, err := cluster.Kubectl(ctx, "get", "events.events.k8s.io", "-n", "argocd", "-o", "json")
		if err != nil {
			return false, fmt.Errorf("listing the Events in argocd: %w\n%s", err, out)
		}
		var list struct {
			Items []tunnelEvent `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			return false, err
		}
		events = nil
		for _, e := range list.Items {
			if e.ReportingController == "iidp.itema.no/database-tunnel" {
				events = append(events, e)
			}
		}
		return len(events) >= n, nil
	}, func() error {
		return fmt.Errorf("the database tunnel recorded %d Events in argocd, want at least %d: %+v; its logs say why (kubectl -n iidp-db-tunnel logs deploy/iidp-db-tunnel)", len(events), n, events)
	})
	if err != nil {
		t.Fatal(err)
	}
	return events
}
