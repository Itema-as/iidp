// Command iidp-db-tunnel is the Database tunnel: the part of the Platform
// through which a developer reaches an Environment's database from their
// own machine, with iidp app db connect. It is configured entirely from
// the environment:
//
//	IIDP_TUNNEL_ORG_ID         the numeric id of the org every Application
//	                           repository belongs to, Itema-as's (required)
//	IIDP_TUNNEL_PLATFORM_REPO  the Platform repository's git URL (default
//	                           https://github.com/Itema-as/iidp-platform.git)
//	IIDP_TUNNEL_GITHUB_API     the GitHub REST API root (default the real one)
//	IIDP_TUNNEL_LISTEN         the listen address (default :8080)
//
// It reads the cluster as the pod's service account, and holds no GitHub
// credential of its own: every check is made with the developer's token.
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

	"github.com/Itema-as/iidp/internal/dbtunnel"
	"github.com/Itema-as/iidp/internal/kubeevent"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformstate"
	"github.com/Itema-as/iidp/internal/version"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("the Database tunnel stopped", "error", err.Error())
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
	orgIDText := env("IIDP_TUNNEL_ORG_ID", "")
	if orgIDText == "" {
		return errors.New("not configured: IIDP_TUNNEL_ORG_ID must be set")
	}
	orgID, err := strconv.ParseInt(orgIDText, 10, 64)
	if err != nil || orgID <= 0 {
		return fmt.Errorf("IIDP_TUNNEL_ORG_ID %q is not a numeric org id", orgIDText)
	}
	cluster, err := platformstate.InCluster()
	if err != nil {
		return fmt.Errorf("the Database tunnel cannot reach the cluster: %w", err)
	}

	tunnel := &dbtunnel.Tunnel{
		OrgID:        orgID,
		PlatformRepo: env("IIDP_TUNNEL_PLATFORM_REPO", platform.RepositoryURL),
		GitHubAPI:    env("IIDP_TUNNEL_GITHUB_API", ""),
		Cluster:      cluster,
		Events:       &kubeevent.Kube{Kube: cluster},
		Log:          log,
	}
	// No read or write timeout: a session is a hijacked connection that
	// lasts as long as the tunnel's own limits let it, and a check is a
	// clone of the Platform repository and a few calls.
	server := &http.Server{
		Addr:              env("IIDP_TUNNEL_LISTEN", ":8080"),
		Handler:           tunnel.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	errs := make(chan error, 1)
	go func() { errs <- server.ListenAndServe() }()
	log.Info("the Database tunnel is listening", "address", server.Addr, "version", version.Version,
		"org_id", orgID, "platform_repository", tunnel.PlatformRepo,
		"idle_limit", dbtunnel.IdleLimit.String(), "max_session", dbtunnel.MaxSession.String())

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Shutdown does not wait for hijacked connections: the open sessions
	// end here, each recording its end.
	tunnel.Shutdown()
	return nil
}
