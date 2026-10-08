package cli_test

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/cli"
	"github.com/Itema-as/iidp/internal/platform"
)

// Itema login on custom domains outside the login cookie domain signs in on
// each host itself, so every path that turns login and such a domain on
// together prints the redirect URIs for the Entra app registration.

// assertLoginCallbacks asserts stdout asks for exactly the redirect URIs of
// hosts, in the az command too, and for none of notHosts. With no hosts it
// asserts the whole block is absent.
func assertLoginCallbacks(t *testing.T, stdout string, hosts []string, notHosts ...string) {
	t.Helper()
	if len(hosts) == 0 {
		if strings.Contains(stdout, "/oauth2/callback") || strings.Contains(stdout, "az ad app") {
			t.Errorf("stdout asks for redirect URIs, want none:\n%s", stdout)
		}
		return
	}
	var uris []string
	for _, host := range hosts {
		uri := "https://" + host + "/oauth2/callback"
		uris = append(uris, uri)
		if !strings.Contains(stdout, "\n  "+uri+"\n") {
			t.Errorf("stdout lacks the redirect URI line %q:\n%s", uri, stdout)
		}
	}
	for _, want := range []string{
		"iidp-oauth2-proxy",
		`app=$(az ad app list --display-name iidp-oauth2-proxy --query '[0].appId' -o tsv)`,
		// Read, add, write: --web-redirect-uris replaces the whole list.
		`az ad app update --id "$app" --web-redirect-uris $( (az ad app show --id "$app" --query 'web.redirectUris[]' -o tsv; printf '%s\n' ` + strings.Join(uris, " ") + `) | sort -u)`,
		"App registrations > iidp-oauth2-proxy > Authentication",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	for _, host := range notHosts {
		if strings.Contains(stdout, "https://"+host+"/oauth2/callback") {
			t.Errorf("stdout asks for %s's redirect URI, which the shared login covers:\n%s", host, stdout)
		}
	}
}

func TestAppCreateLoginWithADomainOutsideTheZone(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--login", "--domain", "x.itma.no", "--domain", "shop.example.com", "--domain", "notitma.no")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "login", "enabled"); got != true {
		t.Errorf("login.enabled = %v, want true", got)
	}
	if got := lookup(t, values, "platform", "loginCookieDomain"); got != "itma.no" {
		t.Errorf("platform.loginCookieDomain = %v, want itma.no", got)
	}
	if got := fmt.Sprint(lookup(t, values, "domains")); got != "[x.itma.no shop.example.com notitma.no]" {
		t.Errorf("domains = %s, want all three", got)
	}
	// The redirect URIs come with the CNAMEs, which the same hosts need.
	if !strings.Contains(stdout, "  CNAME shop.example.com -> shop.app.itma.no\n") {
		t.Errorf("stdout lacks the CNAME line:\n%s", stdout)
	}
	if strings.Index(stdout, "CNAME shop.example.com") > strings.Index(stdout, "/oauth2/callback") {
		t.Errorf("the redirect URIs come before the CNAME lines:\n%s", stdout)
	}
	assertLoginCallbacks(t, stdout, []string{"shop.example.com", "notitma.no"}, "x.itma.no")
}

func TestAppCreateWithoutLoginPrintsNoRedirectURIs(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--domain", "shop.example.com")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	assertLoginCallbacks(t, stdout, nil)
}

func TestAppCreatePathLoginWithADomainOutsideTheZone(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--name", "shop", "--path", "create", "--framework", "nextjs", "--login", "--domain", "shop.example.com")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	if gh.cloneURL(platform.Org, "shop") == "" {
		t.Errorf("the Application repository was not created")
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "login", "enabled"); got != true {
		t.Errorf("login.enabled = %v, want true", got)
	}
	assertLoginCallbacks(t, stdout, []string{"shop.example.com"})
}

func TestAppAdoptLoginWithADomainOutsideTheZone(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)
	gh.seedAdoptRepository(t, platform.Org, "shop", "main", map[string]string{"package.json": nextJSPackageJSON})

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL},
		"--path", "adopt", "--repo", platform.Org+"/shop", "--login", "--domain", "shop.example.com")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "login", "enabled"); got != true {
		t.Errorf("login.enabled = %v, want true", got)
	}
	assertLoginCallbacks(t, stdout, []string{"shop.example.com"})
}

// Without a cloudflareZone the login cookie's domain is baseDomain, as it
// is for the bootstrap's oauth2-proxy, so a host only inside itma.no signs
// in on its own.
func TestAppCreateLoginCookieDomainIsBaseDomainWithoutAZone(t *testing.T) {
	url := newPlatformRepository(t, testPlatformYAML)

	stdout, stderr, code := createApplication(t, url, cli.Dependencies{},
		"--name", "shop", "--kind", "web-service", "--login", "--domain", "x.itma.no", "--domain", "butikk.app.itma.no")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "platform", "loginCookieDomain"); got != "app.itma.no" {
		t.Errorf("platform.loginCookieDomain = %v, want app.itma.no", got)
	}
	assertLoginCallbacks(t, stdout, []string{"x.itma.no"}, "butikk.app.itma.no")
}

// The wizard asks the login question whatever the domains are.
func TestAppCreateWizardAsksLoginForADomainOutsideTheZone(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	gh := newFakeGitHub(t)

	// postgres, staging, domain, login, sign-in groups, size, confirm.
	stdin := "n\nn\nx.itma.no, shop.example.com\ny\n\n\ny\n"

	stdout, stderr, code := createApplicationInteractive(t, url, cli.Dependencies{GitHubAPI: gh.srv.URL}, stdin,
		"--name", "shop", "--path", "create", "--framework", "nextjs")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Itema login?") {
		t.Errorf("stdout does not ask the Itema login question:\n%s", stdout)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "login", "enabled"); got != true {
		t.Errorf("values.yaml login.enabled = %v, want true", got)
	}
	assertLoginCallbacks(t, stdout, []string{"shop.example.com"}, "x.itma.no")
}

func TestAppAddCapabilityLoginWithADomainOutsideTheZoneGivenTogether(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)

	stdout, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--login", "--domain", "x.itma.no", "--domain", "shop.example.com")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if got := fmt.Sprint(lookup(t, values, "domains")); got != "[x.itma.no shop.example.com]" {
		t.Errorf("domains = %s, want both", got)
	}
	assertLoginCallbacks(t, stdout, []string{"shop.example.com"}, "x.itma.no")
}

// The domains already there need the redirect URIs as much as new ones.
func TestAppAddCapabilityLoginWhenADomainOutsideTheZoneIsAlreadyPresent(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--domain", "x.itma.no", "--domain", "shop.example.com")

	stdout, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--login")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if got := lookup(t, values, "login", "enabled"); got != true {
		t.Errorf("login.enabled = %v, want true", got)
	}
	assertLoginCallbacks(t, stdout, []string{"shop.example.com"}, "x.itma.no")
}

// Only the domain added now needs a redirect URI; the ones already there
// were printed when they or login were added.
func TestAppAddCapabilityDomainOutsideTheZoneWithLogin(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--login", "--domain", "old.example.com")

	stdout, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--domain", "shop.example.com")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	values := readYAML(t, filepath.Join(cloneMain(t, url), "applications/shop/prod/values.yaml"))
	if got := fmt.Sprint(lookup(t, values, "domains")); got != "[old.example.com shop.example.com]" {
		t.Errorf("domains = %s, want both", got)
	}
	if got := lookup(t, values, "platform", "loginCookieDomain"); got != "itma.no" {
		t.Errorf("platform.loginCookieDomain = %v, want itma.no", got)
	}
	assertLoginCallbacks(t, stdout, []string{"shop.example.com"}, "old.example.com")
}

func TestAppAddCapabilityDomainOutsideTheZoneWithoutLoginPrintsNoRedirectURIs(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url)

	stdout, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--domain", "shop.example.com")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	assertLoginCallbacks(t, stdout, nil)
}

// Sign-in groups change who gets in, not where they sign in.
func TestAppAddCapabilityLoginGroupPrintsNoRedirectURIs(t *testing.T) {
	url := newPlatformRepository(t, testCapabilitiesPlatformYAML)
	seedApplication(t, url, "--login", "--domain", "shop.example.com")

	stdout, stderr, code := addCapability(t, url, "shop", cli.Dependencies{}, "--login-group", "0f3b6a4e-8c1d-4e2f-9a7b-5c6d7e8f9a0b")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	assertLoginCallbacks(t, stdout, nil)
}

// The help of both commands says what --login does with a custom domain
// outside the zone, and no longer that it is refused.
func TestHelpSaysDomainsOutsideTheZoneSignInOnTheirOwnHost(t *testing.T) {
	for _, command := range [][]string{{"app", "create"}, {"app", "add-capability"}} {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			var out bytes.Buffer
			cli.Run(append(command, "--help"), strings.NewReader(""), &out, &out)
			// Lines are wrapped, so compare with whitespace collapsed.
			help := strings.Join(strings.Fields(out.String()), " ")
			for _, stale := range []string{"refused while", "must then be inside", "go together only"} {
				if strings.Contains(help, stale) {
					t.Errorf("--help still says %q:\n%s", stale, out.String())
				}
			}
			if !strings.Contains(help, "signs in on its own host") || !strings.Contains(help, "redirect URI") {
				t.Errorf("--help does not say a custom domain outside cloudflareZone signs in on its own host and needs a redirect URI:\n%s", out.String())
			}
		})
	}
}
