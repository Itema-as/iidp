package dbtunnel_test

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/Itema-as/iidp/internal/kubeevent"
)

// logLines are the tunnel's JSON log lines.
func (e *env) logLines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(e.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("a log line that is not JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func (e *env) waitForEvents(t *testing.T, n int) []kubeevent.Event {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if events := e.events.all(); len(events) >= n {
			return events
		}
	}
	t.Fatalf("recorded %d Events, want %d: %+v", len(e.events.all()), n, e.events.all())
	return nil
}

func TestASessionIsRecordedAtItsStartAndItsEnd(t *testing.T) {
	e := newEnv(t)
	db := newFakePostgres(t, map[string]string{"shop_write": previewWritePw})
	server := e.serving(t, db)

	conn, frontend := connect(t, server, pushToken, "staging", false)
	receive(t, frontend)
	const sql = "SELECT card_number FROM customers WHERE name = 'Kari Nordmann'"
	frontend.Send(&pgproto3.Query{String: sql})
	_ = frontend.Flush()
	receive(t, frontend)
	frontend.Send(&pgproto3.Terminate{})
	_ = frontend.Flush()
	conn.Close()

	events := e.waitForEvents(t, 2)
	started, ended := events[0], events[1]
	for _, ev := range events {
		if ev.Regarding.Name != "shop-staging" || ev.Regarding.Kind != "Application" || ev.Metadata.Namespace != "argocd" || ev.ReportingController != "iidp.itema.no/database-tunnel" {
			t.Errorf("an Event about %+v in %s from %s, want shop-staging's ArgoCD Application in argocd", ev.Regarding, ev.Metadata.Namespace, ev.ReportingController)
		}
		a := ev.Metadata.Annotations
		if a["iidp.itema.no/login"] != "push-developer" || a["iidp.itema.no/application"] != "shop" || a["iidp.itema.no/environment"] != "staging" || a["iidp.itema.no/role"] != "shop_write" {
			t.Errorf("%s's annotations = %v", ev.Reason, a)
		}
	}
	if started.Reason != "DatabaseSessionStarted" || started.Type != "Normal" || started.Note != "push-developer connected to shop staging's database as shop_write (read-write)" {
		t.Errorf("the start = %+v", started)
	}
	a := ended.Metadata.Annotations
	if ended.Reason != "DatabaseSessionEnded" || a["iidp.itema.no/duration"] == "" || a["iidp.itema.no/bytes-from-client"] == "0" || a["iidp.itema.no/bytes-to-client"] == "0" ||
		!strings.Contains(ended.Note, "push-developer's session on shop staging's database as shop_write ended after") {
		t.Errorf("the end = %+v", ended)
	}

	var startedLine, endedLine map[string]any
	for _, line := range e.logLines(t) {
		switch line["msg"] {
		case "database session started":
			startedLine = line
		case "database session ended":
			endedLine = line
		}
	}
	for name, line := range map[string]map[string]any{"start": startedLine, "end": endedLine} {
		if line["login"] != "push-developer" || line["application"] != "shop" || line["environment"] != "staging" || line["role"] != "shop_write" {
			t.Errorf("the %s's log line = %v", name, line)
		}
	}
	if endedLine["duration"] == nil || endedLine["bytes_from_client"] == nil || endedLine["bytes_to_client"] == nil {
		t.Errorf("the end's log line = %v, want its duration and bytes each way", endedLine)
	}

	logs := e.logs.String()
	for _, never := range []string{sql, "Kari Nordmann", previewWritePw, pushToken} {
		if strings.Contains(logs, never) {
			t.Errorf("the log holds %q:\n%s", never, logs)
		}
		for _, ev := range events {
			if strings.Contains(ev.Note, never) || strings.Contains(strings.Join(values(ev.Metadata.Annotations), " "), never) {
				t.Errorf("an Event holds %q: %+v", never, ev)
			}
		}
	}
}

func TestARefusalIsRecordedOnceTheDeveloperAndEnvironmentAreKnown(t *testing.T) {
	e := newEnv(t)
	server := e.serving(t, newFakePostgres(t, nil))
	e.cluster.put(clusterPath("shop-prod", "shop-db"), clusterObject("admin", "none", role{"shop_write", "shop-db-write"}, role{"shop_read", ""}))

	_, frontend := connect(t, server, pullToken, "prod", false)
	receive(t, frontend)
	events := e.waitForEvents(t, 1)
	refused := events[0]
	if refused.Reason != "DatabaseSessionRefused" || refused.Type != "Warning" || refused.Regarding.Name != "shop-prod" ||
		refused.Metadata.Annotations["iidp.itema.no/login"] != "pull-developer" || !strings.Contains(refused.Note, "pull-developer was refused shop prod's database: your permission on Itema-as/shop is pull") {
		t.Errorf("the refusal = %+v", refused)
	}

	// Neither a token GitHub rejects nor a pull request with no Preview
	// Environment has anyone or anything to record.
	for _, tc := range []struct{ token, environment string }{{revokedToken, "prod"}, {pushToken, "pr-9"}} {
		_, frontend := connect(t, server, tc.token, tc.environment, false)
		receive(t, frontend)
	}
	e.tunnel.WaitForEvents()
	if got := e.events.all(); len(got) != 1 {
		t.Errorf("recorded %d Events, want only the first refusal: %+v", len(got), got)
	}
	refusals := 0
	for _, line := range e.logLines(t) {
		if line["msg"] == "database session refused" {
			refusals++
		}
	}
	if refusals != 3 {
		t.Errorf("%d refusals logged, want all 3", refusals)
	}
	if strings.Contains(e.logs.String(), revokedToken) {
		t.Error("the log holds a token")
	}
}

func values(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// A developer refused for their permission on the repository is recorded
// on the Environment they asked for, auto resolved, once it is one the
// Platform repository has. A Preview Environment is only known by its
// Cluster, which the permission check comes before.
func TestARepositoryRefusalIsRecordedOnTheEnvironment(t *testing.T) {
	e := newEnv(t)
	server := e.serving(t, newFakePostgres(t, nil))
	for _, tc := range []struct{ token, application, environment string }{
		{strangerToken, "shop", "prod"},
		{strangerToken, "shop", "auto"},
		{adminToken, "notes", "prod"},
		{adminToken, "later", "prod"},
		{strangerToken, "shop", "pr-7"},
		{adminToken, "notes", "staging"},
	} {
		if _, err := client(server, tc.token).Check(context.Background(), tc.application, tc.environment, false); err == nil {
			t.Fatalf("%s %s %s was let in", tc.token, tc.application, tc.environment)
		}
	}
	e.tunnel.WaitForEvents()
	var got []string
	for _, ev := range e.events.all() {
		got = append(got, ev.Reason+" "+ev.Regarding.Name+" "+ev.Metadata.Annotations["iidp.itema.no/login"])
	}
	sort.Strings(got)
	want := []string{
		"DatabaseSessionRefused later-prod admin-developer",
		"DatabaseSessionRefused notes-prod admin-developer",
		"DatabaseSessionRefused shop-prod stranger-developer",
		"DatabaseSessionRefused shop-staging stranger-developer",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
