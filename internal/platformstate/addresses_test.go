package platformstate_test

import (
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/platformstate"
)

// ArgoCD writes the Platform address as http://, because its TLS entry
// names no Secret, and a custom domain as https://. Both are served over
// HTTPS, so both are shown as https://. A path, such as that of
// cert-manager's HTTP-01 solver Ingress during a challenge, is not an
// address of its own.
func TestAddressesAreHTTPSOnePerHost(t *testing.T) {
	f := env()
	status(f.app)["summary"] = obj{"externalURLs": []any{
		"http://hello.app.itma.no", "https://hello.itma.no",
		"http://hello.itma.no/.well-known/acme-challenge/token",
	}}
	got := platformstate.EnvironmentOf("shop", f.objects(t), now).Addresses
	if strings.Join(got, " ") != "https://hello.app.itma.no https://hello.itma.no" {
		t.Errorf("addresses = %v, want both hosts once, https", got)
	}
}
