package platformrepo

import (
	"fmt"
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
