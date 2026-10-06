package github_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Itema-as/iidp/internal/github"
)

// serve answers GET path with body, as GitHub would.
func serve(t *testing.T, path, body string) *github.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gho_developer" {
			http.Error(w, `{"message": "Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &github.Client{BaseURL: server.URL, Token: "gho_developer"}
}

// GitHub's permissions object, as GET /repositories/{id} returns it to a
// user, for each of the five repository roles.
func TestRepositoryByIDDecodesEveryPermission(t *testing.T) {
	for _, tc := range []struct {
		permissions                 string
		pull, push, maintain, admin bool
		permission                  string
	}{
		{`{"admin": true, "maintain": true, "push": true, "triage": true, "pull": true}`, true, true, true, true, "admin"},
		{`{"admin": false, "maintain": true, "push": true, "triage": true, "pull": true}`, true, true, true, false, "maintain"},
		{`{"admin": false, "maintain": false, "push": true, "triage": true, "pull": true}`, true, true, false, false, "push"},
		{`{"admin": false, "maintain": false, "push": false, "triage": true, "pull": true}`, true, false, false, false, "pull"},
		{`{"admin": false, "maintain": false, "push": false, "triage": false, "pull": true}`, true, false, false, false, "pull"},
		{`{"admin": false, "maintain": false, "push": false, "triage": false, "pull": false}`, false, false, false, false, ""},
		// No permissions at all, as GitHub answers an anonymous read.
		{`null`, false, false, false, false, ""},
	} {
		gh := serve(t, "/repositories/700000002", `{"id": 700000002, "full_name": "Itema-as/shop", "permissions": `+tc.permissions+`}`)
		repo, err := gh.RepositoryByID(context.Background(), 700000002)
		if err != nil {
			t.Fatal(err)
		}
		got := []bool{repo.CanPull, repo.CanPush, repo.CanMaintain, repo.CanAdmin}
		want := []bool{tc.pull, tc.push, tc.maintain, tc.admin}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("permissions %s: pull, push, maintain, admin = %v, want %v", tc.permissions, got, want)
				break
			}
		}
		if repo.Permission() != tc.permission {
			t.Errorf("permissions %s: Permission() = %q, want %q", tc.permissions, repo.Permission(), tc.permission)
		}
		if repo.FullName != "Itema-as/shop" {
			t.Errorf("FullName = %q", repo.FullName)
		}
	}
}

func TestAuthenticatedLoginIsTheTokensUser(t *testing.T) {
	gh := serve(t, "/user", `{"login": "octocat", "id": 583231}`)
	login, err := gh.AuthenticatedLogin(context.Background())
	if err != nil || login != "octocat" {
		t.Fatalf("AuthenticatedLogin = %q, %v; want octocat", login, err)
	}

	gh.Token = "gho_revoked"
	if _, err := gh.AuthenticatedLogin(context.Background()); !github.IsUnauthorized(err) {
		t.Errorf("a token GitHub rejects: %v, want IsUnauthorized", err)
	}
}
