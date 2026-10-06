package dbtunnel_test

import (
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/dbtunnel"
)

func TestTheCheckGrantsTheRoleTheClusterNames(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		name, token, environment string
		readOnly                 bool
		want                     dbtunnel.Grant
	}{
		{"admin gets read-write on prod", adminToken, "prod", false,
			dbtunnel.Grant{Login: "admin-developer", Application: "shop", Environment: "prod", Namespace: "shop-prod", Cluster: "shop-db", Access: "read-write", Role: "shop_write", Password: shopPassword}},
		{"maintain gets read-only on prod", maintainToken, "prod", false,
			dbtunnel.Grant{Login: "maintain-developer", Application: "shop", Environment: "prod", Namespace: "shop-prod", Cluster: "shop-db", Access: "read-only", Role: "shop_read", Password: readPassword}},
		{"triage counts as pull", triageToken, "prod", false,
			dbtunnel.Grant{Login: "triage-developer", Application: "shop", Environment: "prod", Namespace: "shop-prod", Cluster: "shop-db", Access: "read-only", Role: "shop_read", Password: readPassword}},
		{"--read-only steps admin down", adminToken, "prod", true,
			dbtunnel.Grant{Login: "admin-developer", Application: "shop", Environment: "prod", Namespace: "shop-prod", Cluster: "shop-db", Access: "read-only", Role: "shop_read", Password: readPassword}},
		{"auto is staging when there is one", pushToken, "auto", false,
			dbtunnel.Grant{Login: "push-developer", Application: "shop", Environment: "staging", Namespace: "shop-staging", Cluster: "shop-staging-db", Access: "read-write", Role: "shop_write", Password: previewWritePw}},
		{"a Preview Environment has staging's levels and Secrets", pushToken, "pr-7", false,
			dbtunnel.Grant{Login: "push-developer", Application: "shop", Environment: "pr-7", Namespace: "shop-pr-7", Cluster: "shop-pr-7-db", Access: "read-write", Role: "shop_write", Password: previewWritePw}},
	} {
		got, err := e.check(t, tc.token, "shop", tc.environment, tc.readOnly)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got.Login != tc.want.Login || got.Application != tc.want.Application || got.Environment != tc.want.Environment ||
			got.Namespace != tc.want.Namespace || got.Cluster != tc.want.Cluster || got.Access != tc.want.Access || got.Role != tc.want.Role || got.Password != tc.want.Password {
			t.Errorf("%s: grant = %+v\nwant %+v", tc.name, got, tc.want)
		}
	}
}

// Each refusal names the Application, the repository or Environment, and
// what is missing.
func TestTheCheckRefusesWithAReason(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		name, token, application, environment string
		readOnly                              bool
		status                                int
		wants                                 []string
	}{
		{"a token GitHub rejects", revokedToken, "shop", "prod", false, http.StatusUnauthorized,
			[]string{"GitHub does not accept your token; run gh auth login again"}},
		{"no token at all", "", "shop", "prod", false, http.StatusUnauthorized,
			[]string{"no GitHub token"}},
		{"a developer who cannot read the repository", strangerToken, "shop", "prod", false, http.StatusForbidden,
			[]string{"you cannot read shop's Application repository (Itema-as/shop, repository id 700000002)", "only its readers may reach shop's databases"}},
		{"an Application bound outside the org", adminToken, "notes", "prod", false, http.StatusForbidden,
			[]string{"notes is bound to a repository outside Itema-as (owner id 999)", "iidp app bind notes --repo Itema-as/<repository> --rebind"}},
		{"an Application bound to nothing", adminToken, "later", "prod", false, http.StatusForbidden,
			[]string{"later is not bound to an Application repository, so there is no repository whose readers may reach its databases"}},
		{"an unknown Application", adminToken, "nosuch", "prod", false, http.StatusNotFound,
			[]string{"there is no Application nosuch on the Platform"}},
		{"an unknown Environment", adminToken, "shop", "qa", false, http.StatusBadRequest,
			[]string{`unknown Environment "qa": must be prod, staging or pr-<pull request number>`}},
		{"an unknown pull request", adminToken, "shop", "pr-8", false, http.StatusNotFound,
			[]string{"there is no Preview Environment for pull request 8 of shop"}},
	} {
		_, err := e.check(t, tc.token, tc.application, tc.environment, tc.readOnly)
		var ref *dbtunnel.Refusal
		if !errors.As(err, &ref) || ref.Status != tc.status {
			t.Errorf("%s: %v, want a %d refusal", tc.name, err, tc.status)
			continue
		}
		for _, want := range tc.wants {
			if !strings.Contains(ref.Message, want) {
				t.Errorf("%s: %q\nwant it to say %q", tc.name, ref.Message, want)
			}
		}
	}
}

func TestTheCheckRefusesADeveloperBelowTheLevels(t *testing.T) {
	e := newEnv(t)
	e.cluster.put(clusterPath("shop-prod", "shop-db"), clusterObject("admin", "maintain", role{"shop_write", "shop-db-write"}, role{"shop_read", "shop-db-read"}))
	_, err := e.check(t, pushToken, "shop", "prod", false)
	var ref *dbtunnel.Refusal
	want := "refused: your permission on Itema-as/shop is push, and shop prod's database admits read-write for admin and read-only for maintain. iidp app db access shop --env prod changes who may connect"
	if !errors.As(err, &ref) || ref.Status != http.StatusForbidden || ref.Message != want {
		t.Errorf("push on a prod for maintain and up: %v\nwant %s", err, want)
	}
	// A Preview Environment's levels are staging's, and so is the command.
	e.cluster.put(clusterPath("shop-pr-7", "shop-pr-7-db"), clusterObject("push", "none", role{"shop_write", "shop-staging-db-write"}, role{"shop_read", ""}))
	_, err = e.check(t, pullToken, "shop", "pr-7", false)
	if err == nil || !strings.Contains(err.Error(), "shop pr-7's database admits read-write for push and read-only for nobody (none). iidp app db access shop --env staging changes who may connect") {
		t.Errorf("pull on a preview for push: %v", err)
	}
}

func TestTheCheckRefusesALevelOfNone(t *testing.T) {
	e := newEnv(t)
	e.cluster.put(clusterPath("shop-prod", "shop-db"), clusterObject("none", "none", role{"shop_write", ""}, role{"shop_read", ""}))
	_, err := e.check(t, adminToken, "shop", "prod", false)
	if err == nil || !strings.Contains(err.Error(), "admits read-write for nobody (none) and read-only for nobody (none). iidp app db access shop --env prod changes who may connect") {
		t.Errorf("a closed prod: %v", err)
	}

	e.cluster.put(clusterPath("shop-staging", "shop-staging-db"), clusterObject("push", "none", role{"shop_write", "shop-staging-db-write"}, role{"shop_read", ""}))
	_, err = e.check(t, adminToken, "shop", "staging", true)
	want := "refused: your permission on Itema-as/shop is admin, and shop staging's database has no read-only access (read-only is none, and read-write admits push). Leave out --read-only, or open it with iidp app db access shop --env staging --read-only <level>"
	if err == nil || err.Error() != want {
		t.Errorf("--read-only where read-only is none: %v\nwant %s", err, want)
	}
}

// A level the developer qualifies for whose role the Cluster does not
// have yet, or whose password Secret is not there, or not readable yet,
// is not set up.
func TestTheCheckRefusesALevelNotSetUp(t *testing.T) {
	const want = "no database access set up for %s yet, run `iidp app db access shop --env %s`"
	e := newEnv(t)
	for _, tc := range []struct {
		name, environment string
		readOnly          bool
		setUp             func()
		command           string
	}{
		{"a role the Cluster has as absent", "staging", true, func() {}, "staging"},
		{"a missing password Secret", "prod", false, func() { e.cluster.remove(secretPath("shop-prod", "shop-db-write")) }, "prod"},
		{"a Secret of another role", "prod", true, func() {
			e.cluster.put(secretPath("shop-prod", "shop-db-read"), secretObject("shop_write", shopPassword))
		}, "prod"},
		{"a Preview Environment's, with staging's command", "pr-7", false, func() { e.cluster.remove(secretPath("shop-pr-7", "shop-staging-db-write")) }, "staging"},
	} {
		tc.setUp()
		_, err := e.check(t, adminToken, "shop", tc.environment, tc.readOnly)
		if wantMessage := fmt.Sprintf(want, tc.environment, tc.command); err == nil || err.Error() != wantMessage {
			t.Errorf("%s: %v\nwant %s", tc.name, err, wantMessage)
		}
	}
}

func TestTheCheckRefusesAnEnvironmentWithoutADatabase(t *testing.T) {
	e := newEnv(t)
	e.cluster.remove(clusterPath("shop-staging", "shop-staging-db"))
	_, err := e.check(t, adminToken, "shop", "staging", false)
	if err == nil || !strings.Contains(err.Error(), "shop staging's database is not running yet") {
		t.Errorf("no Cluster yet: %v", err)
	}
	e.cluster.put(clusterPath("shop-staging", "shop-staging-db"), clusterObject("", ""))
	_, err = e.check(t, adminToken, "shop", "staging", false)
	if err == nil || !strings.Contains(err.Error(), "its application chart is older than database access") {
		t.Errorf("a Cluster without access annotations: %v", err)
	}
}

// Only the tunnel links the Postgres driver: the CLI reaches it through
// internal/dbtunnel/api, and nothing else needs it.
func TestOnlyTheTunnelLinksThePostgresDriver(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}
	out, err := exec.Command(gobin, "list", "-deps", "github.com/Itema-as/iidp/cmd/iidp", "github.com/Itema-as/iidp/cmd/iidp-deploy-gate", "github.com/Itema-as/iidp/cmd/iidp-argus").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.HasPrefix(pkg, "github.com/jackc/") {
			t.Errorf("the CLI, the Deploy gate or Argus links %s; only cmd/iidp-db-tunnel may", pkg)
		}
	}
}
