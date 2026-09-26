package platformrepo

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// LoginCookieDomain is the domain the Itema login cookie is set for,
// without the leading dot: CloudflareZone, or BaseDomain when platform.yaml
// has no zone. The bootstrap's oauth2-proxy derives its cookie-domain and
// whitelist-domain from the same two fields
// (bootstrap/templates/_helpers.tpl, iidp-bootstrap.loginCookieDomain), and
// the CLI writes it into every Environment with login as
// platform.loginCookieDomain, which the chart checks custom domains
// against (docs/implementation-notes/76-login-in-zone-domains.md).
func (c Config) LoginCookieDomain() string {
	if c.CloudflareZone != "" {
		return c.CloudflareZone
	}
	return c.BaseDomain
}

// HostsOutsideLoginCookieDomain returns the hosts of domains that are not
// cookieDomain itself or under it, in the order given: the ones the login
// cookie would never reach.
func HostsOutsideLoginCookieDomain(domains []string, cookieDomain string) []string {
	var outside []string
	for _, host := range domains {
		if !inZone(host, cookieDomain) {
			outside = append(outside, host)
		}
	}
	return outside
}

// CheckLoginDomains refuses Itema login together with custom domains that
// are not all inside cfg's login cookie domain, naming the ones outside
// it: --login on app create, or on add-capability for an Application whose
// prod already has domains. The same rule the chart enforces at render
// time (chart/application/templates/_helpers.tpl,
// application.login.checkDomains).
func CheckLoginDomains(cfg Config, domains []string) error {
	cookieDomain := cfg.LoginCookieDomain()
	outside := HostsOutsideLoginCookieDomain(domains, cookieDomain)
	if len(outside) == 0 {
		return nil
	}
	return fmt.Errorf("--login refused, custom domains outside %s: %s. %s", cookieDomain, strings.Join(outside, ", "), loginDomainRule(cookieDomain))
}

// checkDomainsForLogin is CheckLoginDomains for add-capability --domain on
// an Application that already has Itema login.
func checkDomainsForLogin(cfg Config, application string, added []string) error {
	cookieDomain := cfg.LoginCookieDomain()
	outside := HostsOutsideLoginCookieDomain(added, cookieDomain)
	if len(outside) == 0 {
		return nil
	}
	return fmt.Errorf("--domain %s refused: %q has Itema login. %s", strings.Join(outside, ", "), application, loginDomainRule(cookieDomain))
}

func loginDomainRule(cookieDomain string) string {
	return fmt.Sprintf("Itema login needs every custom domain inside %s, the domain its sign-in cookie is set for (%s's cloudflareZone, or baseDomain without one); the cookie never reaches a host outside it", cookieDomain, ConfigFile)
}

// groupIDPattern is an Entra ID object id: a GUID, compared lowercased.
var groupIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// FindGroupIDHelp says where a group's object id is found, for the flag's
// refusals and the wizard's question.
const FindGroupIDHelp = "a group's object id is on its Overview page in the Entra admin center (Groups > All groups), or: az ad group show --group <name> --query id -o tsv"

// NormalizeLoginGroups checks sign-in groups, Entra ID group object ids,
// and returns them lowercased, in the order given: the form the ID token's
// groups claim and Microsoft Graph use, which oauth2-proxy compares as
// strings. Only the shape is checked. The CLI has no Entra access, so a
// GUID that is no group of Itema's is not caught; it lets nobody in. An id
// that is not a GUID, or one given twice, is refused, naming it. The chart
// applies the same rule (chart/application/templates/_helpers.tpl,
// application.login.groups).
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
