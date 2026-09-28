package platformstate

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// grafanaLogsDatasource is the uid Grafana Cloud gives a stack's own Loki
// data source.
const grafanaLogsDatasource = "grafanacloud-logs"

// LinksOf are where to look at env beyond its state: its ArgoCD
// Application under argocdURL, and its logs in Grafana Cloud's Explore
// under grafanaURL, by namespace. Either is left out when its base is
// empty or env lacks what it needs; nil when both are. The Deploy gate
// takes the bases from platform.yaml, Argus from its bootstrap values.
func LinksOf(argocdURL, grafanaURL string, env Environment) *Links {
	var l Links
	if argocdURL != "" && env.ArgoCD != nil {
		l.ArgoCD = strings.TrimSuffix(argocdURL, "/") + "/applications/" + ArgoCDNamespace + "/" + url.PathEscape(env.ArgoCD.Application)
	}
	if grafanaURL != "" && env.Namespace != "" {
		datasource := map[string]string{"type": "loki", "uid": grafanaLogsDatasource}
		panes, _ := json.Marshal(map[string]any{"logs": map[string]any{
			"datasource": grafanaLogsDatasource,
			"queries":    []any{map[string]any{"refId": "A", "expr": fmt.Sprintf("{namespace=%q}", env.Namespace), "datasource": datasource}},
			"range":      map[string]string{"from": "now-1h", "to": "now"},
		}})
		l.Grafana = strings.TrimSuffix(grafanaURL, "/") + "/explore?schemaVersion=1&panes=" + url.QueryEscape(string(panes))
	}
	if l == (Links{}) {
		return nil
	}
	return &l
}
