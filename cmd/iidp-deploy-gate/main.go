// Command iidp-deploy-gate is the Deploy gate: the service on the Platform
// through which an Application repository's CI deploys and promotes
// (internal/deploygate). The bootstrap chart runs it
// (bootstrap/components/deploy-gate); it is configured entirely from the
// environment:
//
//	IIDP_GATE_AUDIENCE       the gate's own URL, https://deploy.<baseDomain>: the
//	                         audience every OIDC token must carry (required)
//	IIDP_GATE_ORG_ID         the numeric id of the org whose repositories may
//	                         deploy, Itema-as's (required)
//	IIDP_GATE_APP_DIR        the directory holding the GitHub App credential's
//	                         githubAppID, githubAppInstallationID and
//	                         githubAppPrivateKey files (required)
//	IIDP_GATE_GHCR_DIR       the directory holding the GHCR pull token's username
//	                         and token files, which the gate checks private
//	                         images with (required)
//	IIDP_GATE_OIDC_ISSUER   the token issuer (default GitHub Actions')
//	IIDP_GATE_OIDC_JWKS_URL  where the issuer's keys are (default
//	                         <issuer>/.well-known/jwks)
//	IIDP_GATE_PLATFORM_REPO  the Platform repository's git URL (default
//	                         https://github.com/Itema-as/iidp-platform.git)
//	IIDP_GATE_GITHUB_API     the GitHub REST API root (default the real one)
//	IIDP_GATE_LISTEN         the listen address (default :8080)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Itema-as/iidp/internal/deploygate"
	"github.com/Itema-as/iidp/internal/oidc"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/registry"
	"github.com/Itema-as/iidp/internal/version"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("the Deploy gate stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	env := func(name, fallback string) string {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
		return fallback
	}
	audience := env("IIDP_GATE_AUDIENCE", "")
	appDir := env("IIDP_GATE_APP_DIR", "")
	orgIDText := env("IIDP_GATE_ORG_ID", "")
	ghcrDir := env("IIDP_GATE_GHCR_DIR", "")
	var missing []string
	for name, v := range map[string]string{"IIDP_GATE_AUDIENCE": audience, "IIDP_GATE_APP_DIR": appDir, "IIDP_GATE_ORG_ID": orgIDText, "IIDP_GATE_GHCR_DIR": ghcrDir} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("not configured: %s must be set", strings.Join(missing, ", "))
	}
	orgID, err := strconv.ParseInt(orgIDText, 10, 64)
	if err != nil || orgID <= 0 {
		return fmt.Errorf("IIDP_GATE_ORG_ID %q is not a numeric org id", orgIDText)
	}
	issuer := env("IIDP_GATE_OIDC_ISSUER", oidc.GitHubActionsIssuer)

	gate := &deploygate.Gate{
		OIDC: &oidc.Verifier{
			Issuer:   issuer,
			JWKSURL:  env("IIDP_GATE_OIDC_JWKS_URL", strings.TrimSuffix(issuer, "/")+"/.well-known/jwks"),
			Audience: audience,
		},
		OrgID:        orgID,
		PlatformRepo: env("IIDP_GATE_PLATFORM_REPO", platform.RepositoryURL),
		GitHubAPI:    env("IIDP_GATE_GITHUB_API", ""),
		Credentials:  deploygate.CredentialsFromDir(appDir),
		Images: &registry.Checker{
			CredentialHost: deploygate.GHCR,
			Credential:     deploygate.RegistryCredentialFromDir(ghcrDir),
		},
		Log: log,
	}
	if _, err := gate.Credentials(); err != nil {
		return err
	}
	if _, err := gate.Images.Credential(); err != nil {
		return err
	}

	server := &http.Server{
		Addr:              env("IIDP_GATE_LISTEN", ":8080"),
		Handler:           gate.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// A deploy clones and pushes the Platform repository, twice when
		// main moved, and may queue behind another one.
		WriteTimeout: 5 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	errs := make(chan error, 1)
	go func() { errs <- server.ListenAndServe() }()
	log.Info("the Deploy gate is listening", "address", server.Addr, "version", version.Version,
		"audience", audience, "issuer", issuer, "org_id", orgID, "platform_repository", gate.PlatformRepo)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
