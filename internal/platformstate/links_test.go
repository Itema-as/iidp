package platformstate

import (
	"net/url"
	"strings"
	"testing"
)

func TestLinksOf(t *testing.T) {
	env := Environment{Name: "prod", Namespace: "shop-prod", ArgoCD: &ArgoCD{Application: "shop-prod"}}

	l := LinksOf("https://argocd.example.test/", "https://itema.grafana.net", env)
	if l == nil || l.ArgoCD != "https://argocd.example.test/applications/argocd/shop-prod" {
		t.Fatalf("links = %+v", l)
	}
	u, err := url.Parse(l.Grafana)
	if err != nil || u.Host != "itema.grafana.net" || u.Path != "/explore" || !strings.Contains(u.Query().Get("panes"), `{namespace=\"shop-prod\"}`) {
		t.Errorf("grafana = %q (%v)", l.Grafana, err)
	}

	if l := LinksOf("", "", env); l != nil {
		t.Errorf("no bases: links = %+v, want nil", l)
	}
	if l := LinksOf("https://argocd.example.test", "https://itema.grafana.net", Environment{Name: "prod"}); l != nil {
		t.Errorf("no ArgoCD Application and no namespace: links = %+v, want nil", l)
	}
	if l := LinksOf("", "https://itema.grafana.net", env); l == nil || l.ArgoCD != "" || l.Grafana == "" {
		t.Errorf("Grafana only: links = %+v", l)
	}
}
