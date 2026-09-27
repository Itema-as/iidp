package platformstate

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Lister lists Kubernetes objects: a GET of a collection path such as
// /api/v1/namespaces/shop-prod/pods, narrowed by a label selector, whose
// items are decoded into into (a pointer to a slice).
type Lister interface {
	List(ctx context.Context, path, labelSelector string, into any) error
}

// Read reads the state of every Environment of application the cluster
// has an ArgoCD Application for: one list of ArgoCD Applications by label,
// then, in each Environment's namespace, its Deployments, their pods, its
// Jobs and its CronJobs. The Environments come back in SortEnvironments
// order, interpreted as of now. It lists no Events, so a Deploy is found
// by its tag, from Applying on.
func Read(ctx context.Context, cluster Lister, application string) ([]Environment, error) {
	now := time.Now()
	var apps []ArgoCDApplication
	if err := cluster.List(ctx, "/apis/argoproj.io/v1alpha1/namespaces/"+ArgoCDNamespace+"/applications", ApplicationLabel+"="+application, &apps); err != nil {
		return nil, err
	}
	envs := []Environment{}
	for _, app := range apps {
		o := Objects{ArgoCD: app}
		ns := app.Spec.Destination.Namespace
		if ns != "" {
			selector := ApplicationLabel + "=" + application
			base := "/namespaces/" + url.PathEscape(ns)
			if err := cluster.List(ctx, "/apis/apps/v1"+base+"/deployments", selector, &o.Deployments); err != nil {
				return nil, err
			}
			if d, ok := workload(o.Deployments); ok && len(d.Spec.Selector.MatchLabels) > 0 {
				if err := cluster.List(ctx, "/api/v1"+base+"/pods", selectorString(d.Spec.Selector.MatchLabels), &o.Pods); err != nil {
					return nil, err
				}
			}
			if err := cluster.List(ctx, "/apis/batch/v1"+base+"/jobs", selector, &o.Jobs); err != nil {
				return nil, err
			}
			if err := cluster.List(ctx, "/apis/batch/v1"+base+"/cronjobs", selector, &o.CronJobs); err != nil {
				return nil, err
			}
		}
		envs = append(envs, EnvironmentOf(application, o, now))
	}
	SortEnvironments(envs)
	return envs, nil
}

func selectorString(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// The files and variables a pod's service account token is mounted as.
const (
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
)

// Kube lists objects from the Kubernetes API with the standard library,
// authenticated as the pod's service account. It does only what Lister
// needs, which keeps client-go out of the Deploy gate.
type Kube struct {
	// BaseURL is the API server, https://<host>:<port>.
	BaseURL string
	// Token yields the bearer token for each request. The in-cluster token
	// is a projected one the kubelet rotates, so it is read every time.
	Token func() (string, error)
	// HTTPClient makes the requests, trusting the cluster's CA.
	HTTPClient *http.Client
}

// InCluster is a Kube for the pod it runs in: the API server from
// KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT, the CA and token
// from the mounted service account. It fails when the pod has no service
// account token mounted.
func InCluster() (*Kube, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a Kubernetes pod: KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set")
	}
	ca, err := os.ReadFile(filepath.Join(serviceAccountDir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("no service account mounted: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("the service account's ca.crt holds no certificate")
	}
	tokenFile := filepath.Join(serviceAccountDir, "token")
	token := func() (string, error) {
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("reading the service account token: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	if _, err := token(); err != nil {
		return nil, err
	}
	return &Kube{
		BaseURL: "https://" + net.JoinHostPort(host, port),
		Token:   token,
		HTTPClient: &http.Client{
			Timeout:   20 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, nil
}

// StatusError is a refusal from the API server.
type StatusError struct {
	Path   string
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("the Kubernetes API answered HTTP %d for %s: %s", e.Status, e.Path, e.Body)
}

// List implements Lister.
func (k *Kube) List(ctx context.Context, path, labelSelector string, into any) error {
	u := k.BaseURL + path
	if labelSelector != "" {
		u += "?labelSelector=" + url.QueryEscape(labelSelector)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if k.Token != nil {
		token, err := k.Token()
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := k.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("listing %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return &StatusError{Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	var list struct {
		Items json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&list); err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	if len(list.Items) == 0 || string(list.Items) == "null" {
		return nil
	}
	if err := json.Unmarshal(list.Items, into); err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	return nil
}
