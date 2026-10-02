// Command iidp-argus is Argus, the live view of the Platform: it watches the
// cluster with client-go's dynamic informers, interprets what it sees with
// internal/platformstate, and streams the result to browsers over server-sent
// events. It only reads the cluster.
//
// It is the only binary here that links client-go, and only the dynamic
// client: no typed clientsets, and no ArgoCD or CloudNativePG modules.
//
// It is configured from the environment:
//
//	IIDP_ARGUS_LISTEN               the listen address (default :8080)
//	IIDP_ARGUS_ARGOCD_URL           ArgoCD's address
//	IIDP_ARGUS_GRAFANA_URL          Grafana's address
//	IIDP_ARGUS_PLATFORM_REPOSITORY  the Platform repository's address
//	IIDP_ARGUS_BOOTSTRAP_REPOSITORY the bootstrap chart's repository
//	IIDP_ARGUS_BOOTSTRAP_REVISION   the bootstrap revision the Platform pins
//
// All but the listen address are only where the detail card links out to
// (argus.Platform). Each may be empty, and the card then leaves that link out.
package main

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/Itema-as/iidp/internal/argus"
	"github.com/Itema-as/iidp/internal/version"
)

// web is the frontend, served at /.
//
//go:embed web
var web embed.FS

// The probe that tells Argus whether it can still see the cluster.
const (
	probeEvery   = 5 * time.Second
	probeTimeout = 3 * time.Second
	// syncTimeout is how long the first look at the cluster may take
	// before Argus shows what it has.
	syncTimeout = time.Minute
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("Argus stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	// client-go logs through klog; send it to the same JSON log.
	klog.SetSlogLogger(log)
	listen := strings.TrimSpace(os.Getenv("IIDP_ARGUS_LISTEN"))
	if listen == "" {
		listen = ":8080"
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	config.UserAgent = "iidp-argus/" + version.Version
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	httpClient, err := rest.HTTPClientFor(config)
	if err != nil {
		return err
	}
	files, err := fs.Sub(web, "web")
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	store := argus.NewStore(time.Now, log)
	store.SetPlatform(argus.NewPlatform(os.Getenv("IIDP_ARGUS_ARGOCD_URL"), os.Getenv("IIDP_ARGUS_GRAFANA_URL"),
		os.Getenv("IIDP_ARGUS_PLATFORM_REPOSITORY"), os.Getenv("IIDP_ARGUS_BOOTSTRAP_REPOSITORY"), os.Getenv("IIDP_ARGUS_BOOTSTRAP_REVISION")))
	go store.Run(ctx, argus.TickEvery)
	informers := startInformers(ctx, client, store, log)
	go func() {
		waitForSync(ctx, informers, syncTimeout, log)
		if ctx.Err() == nil {
			store.Seed()
			log.Info("Argus has read the cluster")
		}
	}()
	go probe(ctx, httpClient, config.Host, store, log)

	server := &http.Server{
		Addr:              listen,
		Handler:           (&argus.Server{Store: store, Web: files}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: /events streams for as long as the browser
		// stays. Its requests end with ctx, so a SIGTERM closes every
		// stream at once and Shutdown need not wait for them.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	errs := make(chan error, 1)
	go func() { errs <- server.ListenAndServe() }()
	log.Info("Argus is listening", "address", listen, "version", version.Version)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	// Streams end with the server; their browsers reconnect to the next Argus.
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// probe asks the API server's /readyz every probeEvery and tells the store
// whether it answered: informers keep their last state and retry quietly while
// the API server is away, so this is what says the picture is no longer live.
// /readyz is open to every authenticated caller (system:public-info-viewer).
func probe(ctx context.Context, client *http.Client, host string, store *argus.Store, log *slog.Logger) {
	url := strings.TrimSuffix(host, "/") + "/readyz"
	ticker := time.NewTicker(probeEvery)
	defer ticker.Stop()
	was := true
	for {
		ok := func() bool {
			ctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return false
			}
			resp, err := client.Do(req)
			if err != nil {
				return false
			}
			resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		}()
		if ctx.Err() != nil {
			return
		}
		if ok != was {
			log.Info("the API server's reachability changed", "reachable", ok)
			was = ok
		}
		store.SetReachable(ok)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
