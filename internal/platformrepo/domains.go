package platformrepo

import (
	"fmt"
	"regexp"
	"strings"
)

// hostnamePattern is the same shape
// chart/application/templates/_helpers.tpl's application.domains helper
// requires at render time: lowercase letters, digits and dashes in
// dot-separated labels, at least two labels. Kept in sync by hand; the
// chart is the source of truth (docs/implementation-notes/08-chart-static-domains-secrets.md
// "What is refused?").
var hostnamePattern = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// DomainPlan is one validated custom domain and how it will be served, for
// printing in the closing summary.
type DomainPlan struct {
	// Host is the hostname, as given.
	Host string
	// Wildcard is true when the Platform's wildcard certificate for
	// baseDomain covers Host (a single label directly under it); false
	// means the chart issues Host its own certificate from
	// platform.httpIssuer instead.
	Wildcard bool
	// Automated is true when external-dns can create Host's DNS record on
	// its own, because Host is baseDomain's wildcard-covered shape or sits
	// inside cloudflareZone; false means the closing summary must print a
	// CNAME to create by hand.
	Automated bool
}

// ValidateDomains validates candidate custom domains for a prod Environment
// against the same rules the chart enforces at render time (a valid
// lowercase DNS hostname of at least two labels, no duplicates), plus one
// only the CLI can check: none may equal a Platform address of any
// Environment the Application has. platformAddresses are those addresses'
// bare hostnames (no scheme), prod and staging alike.
//
// It takes baseDomain, cloudflareZone and platformAddresses as plain values
// rather than a Config and an Application, so issue #17's add-capability
// can call it again for an Environment that already exists, not only a
// fresh one.
func ValidateDomains(domains []string, baseDomain, cloudflareZone string, platformAddresses []string) ([]DomainPlan, error) {
	seen := make(map[string]bool, len(domains))
	plans := make([]DomainPlan, 0, len(domains))
	for _, host := range domains {
		if len(host) > 253 || !hostnamePattern.MatchString(host) {
			return nil, fmt.Errorf("--domain %q is not a valid DNS hostname: lowercase letters, digits and dashes in dot-separated labels, at least two labels", host)
		}
		for _, addr := range platformAddresses {
			if host == addr {
				return nil, fmt.Errorf("--domain %q is a Platform address of this Application; remove it", host)
			}
		}
		for _, reserved := range ReservedNames {
			if host == reserved+"."+baseDomain {
				return nil, fmt.Errorf("--domain %q is the Platform's own address; it cannot be an Application's", host)
			}
		}
		if seen[host] {
			return nil, fmt.Errorf("--domain %q is listed twice", host)
		}
		seen[host] = true

		wildcard := isWildcardHost(host, baseDomain)
		plans = append(plans, DomainPlan{
			Host:      host,
			Wildcard:  wildcard,
			Automated: wildcard || inZone(host, cloudflareZone),
		})
	}
	return plans, nil
}

// isWildcardHost reports whether host is covered by the Platform's wildcard
// certificate for baseDomain: exactly one label directly under it, the same
// rule chart/application/templates/_helpers.tpl's application.domains uses.
func isWildcardHost(host, baseDomain string) bool {
	suffix := "." + baseDomain
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	return !strings.Contains(strings.TrimSuffix(host, suffix), ".")
}

// inZone reports whether host is zone itself or sits anywhere under it, the
// shape external-dns can create a record for.
func inZone(host, zone string) bool {
	if zone == "" {
		return false
	}
	return host == zone || strings.HasSuffix(host, "."+zone)
}
