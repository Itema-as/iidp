package argus

import (
	"strings"
	"time"

	"github.com/Itema-as/iidp/internal/platformstate"
)

// Platform is the addresses the browser's detail card links out to. Argus
// reads none of them itself: the bootstrap passes them in
// (bootstrap/templates/argus.yaml), and an empty field is a link the card
// leaves out.
type Platform struct {
	// ArgoCDURL is ArgoCD's address, such as https://argocd.app.itma.no.
	ArgoCDURL string `json:"argocdURL,omitempty"`
	// GrafanaURL is the Grafana Cloud stack's address, such as
	// https://itema.grafana.net.
	GrafanaURL string `json:"grafanaURL,omitempty"`
	// PlatformRepository is the Platform repository's web address, such
	// as https://github.com/Itema-as/iidp-platform: a Deploy's commit is
	// one of its commits.
	PlatformRepository string `json:"platformRepository,omitempty"`
	// BootstrapRepository and BootstrapRevision are the repository the
	// bootstrap chart comes from and the revision the Platform pins, for
	// where a Platform component is installed.
	BootstrapRepository string `json:"bootstrapRepository,omitempty"`
	BootstrapRevision   string `json:"bootstrapRevision,omitempty"`
}

// NewPlatform is a Platform from the addresses the bootstrap passes in,
// each trimmed of a trailing slash, and the repositories of ".git", so
// that they are web addresses to put paths after.
func NewPlatform(argocdURL, grafanaURL, platformRepository, bootstrapRepository, bootstrapRevision string) Platform {
	web := func(s string) string {
		s = strings.TrimSuffix(strings.TrimSpace(s), "/")
		return strings.TrimSuffix(s, ".git")
	}
	return Platform{
		ArgoCDURL:           web(argocdURL),
		GrafanaURL:          web(grafanaURL),
		PlatformRepository:  web(platformRepository),
		BootstrapRepository: web(bootstrapRepository),
		BootstrapRevision:   strings.TrimSpace(bootstrapRevision),
	}
}

// withLinks gives each Environment of app its links, from the
// Platform's ArgoCD and Grafana, as the Deploy gate gives them to iidp
// app status.
func withLinks(app platformstate.Application, p Platform) platformstate.Application {
	for i := range app.Environments {
		env := &app.Environments[i]
		env.Links = platformstate.LinksOf(p.ArgoCDURL, p.GrafanaURL, env.Environment)
	}
	return app
}

// deployedAt is when the image an Environment runs was deployed, which
// iidp app status reads from the Platform repository and Argus cannot.
// Argus has it from the Deploy that brought the image: the newest one of
// that tag that is serving, with the Deploy gate's commit, joined to the
// entry of ArgoCD's history that synced that commit (else the time the
// gate accepted it). The gate's Event lasts an hour, so known holds what
// was found for as long as the Environment keeps running that tag; it is
// keyed by the ArgoCD Application and the tag, and it only knows what
// this run of Argus saw.
func deployedAt(env platformstate.EnvironmentState, app platformstate.ArgoCDApplication, known map[string]time.Time) *time.Time {
	if env.Image == nil || env.Image.Tag == "" {
		return nil
	}
	key := app.Metadata.Name + "@" + env.Image.Tag
	for i := len(env.Deploys) - 1; i >= 0; i-- { // newest first
		d := env.Deploys[i]
		if d.Tag != env.Image.Tag || d.Hop != platformstate.HopServing || d.Refused {
			continue
		}
		at := d.At
		if d.Commit != "" {
			for _, h := range app.Status.History {
				if h.DeployedAt != nil && anyRevision(d.Commit, h.Revision, h.Revisions) {
					at = h.DeployedAt
				}
			}
		}
		if at != nil {
			known[key] = *at
			return at
		}
	}
	if at, ok := known[key]; ok {
		return &at
	}
	return nil
}

// anyRevision reports whether commit is revision or one of revisions; a
// short commit matches the full one it abbreviates.
func anyRevision(commit, revision string, revisions []string) bool {
	for _, r := range append([]string{revision}, revisions...) {
		a, b := commit, r
		if len(a) > len(b) {
			a, b = b, a
		}
		if len(a) >= 7 && strings.HasPrefix(b, a) {
			return true
		}
	}
	return false
}
