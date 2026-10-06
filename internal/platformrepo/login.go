package platformrepo

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// LoginCookieDomain is the domain the Itema login cookie is set for,
// without the leading dot: CloudflareZone, or BaseDomain when platform.yaml
// has no zone. The bootstrap's oauth2-proxy derives its cookie domain the
// same way (iidp-bootstrap.loginCookieDomain), and the chart decides by it
// which login each custom domain signs in through.
func (c Config) LoginCookieDomain() string {
	if c.CloudflareZone != "" {
		return c.CloudflareZone
	}
	return c.BaseDomain
}

// HostsOutsideLoginCookieDomain returns the hosts of domains that are not
// cookieDomain itself or under it, in the order given: the ones the shared
// login cookie never reaches, which sign in on their own host instead.
func HostsOutsideLoginCookieDomain(domains []string, cookieDomain string) []string {
	var outside []string
	for _, host := range domains {
		if !inZone(host, cookieDomain) {
			outside = append(outside, host)
		}
	}
	return outside
}

// LoginCallbackURL is the sign-in callback of a custom domain outside the
// login cookie domain. The bootstrap's host-only oauth2-proxy serves it on
// the host itself, so the Entra app registration must list it as a redirect
// URI.
func LoginCallbackURL(host string) string {
	return "https://" + host + "/oauth2/callback"
}

// groupIDPattern is an Entra ID object id: a GUID, compared lowercased.
var groupIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// FindGroupIDHelp says where a group's object id is found, for the flag's
// refusals and the wizard's question.
const FindGroupIDHelp = "a group's object id is on its Overview page in the Entra admin center (Groups > All groups), or: az ad group show --group <name> --query id -o tsv"

// NormalizeLoginGroups checks sign-in groups, Entra ID group object ids,
// and returns them lowercased, in the order given: oauth2-proxy compares
// them as strings with the ID token's groups claim. Only the shape is
// checked, since the CLI has no Entra access. An id that is not a GUID, or
// one given twice, is refused. The chart applies the same rule.
func NormalizeLoginGroups(ids []string) ([]string, error) {
	groups := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		group := strings.ToLower(strings.TrimSpace(id))
		if !groupIDPattern.MatchString(group) {
			return nil, fmt.Errorf("--login-group %q is not an Entra group object id, a GUID such as 0f3b6a4e-8c1d-4e2f-9a7b-5c6d7e8f9a0b; %s", id, FindGroupIDHelp)
		}
		if seen[group] {
			return nil, fmt.Errorf("--login-group %s is given twice", group)
		}
		seen[group] = true
		groups = append(groups, group)
	}
	return groups, nil
}

// ErrLoginGroupsWithoutLogin is wrapped when sign-in groups are asked for
// an Application that does not have, and is not given, Itema login.
var ErrLoginGroupsWithoutLogin = errors.New("sign-in groups need Itema login")
