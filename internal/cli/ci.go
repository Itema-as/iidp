package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Itema-as/iidp/internal/appconfig"
	"github.com/Itema-as/iidp/internal/deploygate"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
)

// DeployGateURLEnvVar is where ci set-image finds the Deploy gate: the
// generated deploy workflow sets it to https://deploy.<baseDomain>,
// rendered in by iidp app create, since CI cannot read the private
// Platform repository to find it (docs/implementation-notes/60-deploy-gate.md).
const DeployGateURLEnvVar = "IIDP_DEPLOY_GATE_URL"

// The two variables GitHub Actions sets in a job with
// permissions: id-token: write, through which the job asks for an OIDC
// token.
const (
	actionsTokenURLEnvVar   = "ACTIONS_ID_TOKEN_REQUEST_URL"
	actionsTokenTokenEnvVar = "ACTIONS_ID_TOKEN_REQUEST_TOKEN"
)

// ciSetImageOptions are the arguments of ci set-image.
type ciSetImageOptions struct {
	application string
	environment string
	tag         string
	gateURL     string
}

func newCICommand(deps Dependencies) *cobra.Command {
	ci := &cobra.Command{
		Use:   "ci",
		Short: "Commands the deploy workflow runs; not for developers",
	}
	ci.AddCommand(newCISetImageCommand(deps))
	return ci
}

func newCISetImageCommand(deps Dependencies) *cobra.Command {
	var opts ciSetImageOptions
	cmd := &cobra.Command{
		Use:   "set-image <app> <prod|staging|auto> <tag>",
		Short: "Ask the Deploy gate to run an image in an Environment (run by the deploy workflow)",
		Long: "Asks the Deploy gate to write tag into image.tag of an Application's\n" +
			"Environment in the Platform repository (" + platform.Repository + "): a\n" +
			"commit SHA on every push to main, a version on a v* tag. auto lets the gate\n" +
			"decide: main deploys to staging when the Application has one and to prod\n" +
			"otherwise, and a v* tag promotes to prod.\n\n" +
			"Runs only in GitHub Actions, in a job with permissions: id-token: write. It\n" +
			"requests an OIDC token for the gate's URL and calls the gate with it; the\n" +
			"gate checks that the token comes from the Application's own repository and\n" +
			"a ref allowed to deploy that Environment, and commits the change itself. The\n" +
			"gate's URL is --gate-url, or " + DeployGateURLEnvVar + ", which the generated\n" +
			"workflow sets. No secret is needed; gh auth login is not consulted.\n\n" +
			"Run it in the checkout of the commit being deployed or promoted: it reads\n" +
			"migrationCommand and tasks from " + appconfig.FileName + " there and sends them with the tag,\n" +
			"so the gate sets the Environment's migration command and Scheduled tasks in\n" +
			"the same commit. An " + appconfig.FileName + " without migrationCommand clears it; with no\n" +
			appconfig.FileName + " at all, the Environment's migration command is left as it is.\n" +
			"No tasks, or no " + appconfig.FileName + ", removes the Environment's Scheduled tasks.",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.application = args[0]
			opts.environment = args[1]
			opts.tag = args[2]
			return runCISetImage(cmd, opts, deps)
		},
	}
	cmd.Flags().StringVar(&opts.gateURL, "gate-url", "", "The Deploy gate's URL, https://deploy.<baseDomain> (default $"+DeployGateURLEnvVar+")")
	return cmd
}

func runCISetImage(cmd *cobra.Command, opts ciSetImageOptions, deps Dependencies) error {
	if err := platformrepo.ValidateName(opts.application); err != nil {
		return err
	}
	if err := validateCIEnvironment(opts.environment); err != nil {
		return err
	}
	if strings.TrimSpace(opts.tag) == "" {
		return errors.New("the tag must not be empty")
	}
	gate := strings.TrimSuffix(strings.TrimSpace(opts.gateURL), "/")
	if gate == "" {
		gate = strings.TrimSuffix(strings.TrimSpace(os.Getenv(DeployGateURLEnvVar)), "/")
	}
	if gate == "" {
		return fmt.Errorf("the Deploy gate's URL is not set: pass --gate-url or set %s to https://deploy.<baseDomain>. A deploy workflow generated before the Deploy gate lacks it; see docs/implementation-notes/60-deploy-gate.md for moving it over", DeployGateURLEnvVar)
	}
	if u, err := url.Parse(gate); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("the Deploy gate's URL %q is not an http(s) URL", gate)
	}

	// The workflow runs this in the checkout of the commit it deploys or
	// promotes, so the migration command and tasks read here are that
	// commit's.
	migrationCommand, tasks, err := readAppConfig()
	if err != nil {
		return err
	}
	if err := appconfig.CheckTaskNamesFit(opts.application, tasks); err != nil {
		return fmt.Errorf("%s: %w", appconfig.FileName, err)
	}

	ctx := cmd.Context()
	client := &http.Client{Timeout: 3 * time.Minute}
	token, err := requestOIDCToken(ctx, client, gate)
	if err != nil {
		return err
	}
	res, err := callDeployGate(ctx, client, gate, token, deploygate.Request{
		Application:      opts.application,
		Environment:      opts.environment,
		Tag:              opts.tag,
		MigrationCommand: migrationCommand,
		Tasks:            tasks,
	}, deps.CIRetryDelay)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	switch {
	case migrationCommand == nil:
		fmt.Fprintf(out, "No %s here, so the migration command on the Platform is left as it is.\n", appconfig.FileName)
	case *migrationCommand == "":
		fmt.Fprintf(out, "%s sets no migration command.\n", appconfig.FileName)
	default:
		fmt.Fprintf(out, "Migration command from %s: %s\n", appconfig.FileName, *migrationCommand)
	}
	if len(tasks) == 0 {
		fmt.Fprintf(out, "No Scheduled tasks in %s.\n", appconfig.FileName)
	} else {
		fmt.Fprintf(out, "Scheduled tasks from %s, on Europe/Oslo time:\n", appconfig.FileName)
		for _, task := range tasks {
			fmt.Fprintf(out, "  %s (%s): %s\n", task.Name, task.Schedule, task.Command)
		}
	}
	if res.Unchanged {
		fmt.Fprintf(out, "%s %s already runs %s with this migration command and these Scheduled tasks; nothing to deploy.\n", res.Application, res.Environment, res.Tag)
		return nil
	}
	fmt.Fprintf(out, "Deploy %s %s %s\n", res.Application, res.Environment, res.Tag)
	if res.MigrationCommandChanged {
		fmt.Fprintf(out, "The migration command changed with it.\n")
	}
	if res.TasksChanged {
		fmt.Fprintf(out, "The Scheduled tasks changed with it.\n")
	}
	fmt.Fprintf(out, "\nThe Deploy gate committed %s to %s:\n  %s\n", shortCommit(res.Commit), platform.Repository, res.File)
	return nil
}

// readAppConfig reads iidp.yaml in the current directory. The migration
// command is nil when there is none (the gate then leaves the
// Environment's command as it is), "" when it sets none (the gate clears
// it), and the command otherwise. The tasks are nil without the file or
// without tasks, which the gate takes as "remove them".
func readAppConfig() (*string, []appconfig.Task, error) {
	f, ok, err := appconfig.Read(".")
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, nil
	}
	return &f.MigrationCommand, f.Tasks, nil
}

// validateCIEnvironment refuses anything but the three values ci set-image
// accepts, clearly, before any network call.
func validateCIEnvironment(environment string) error {
	switch environment {
	case "prod", "staging", platformrepo.EnvironmentAuto:
		return nil
	default:
		return fmt.Errorf("unknown Environment %q: must be prod, staging or auto", environment)
	}
}

// requestOIDCToken asks GitHub Actions for an OIDC token whose audience is
// the gate's URL, the only audience the gate accepts.
func requestOIDCToken(ctx context.Context, client *http.Client, audience string) (string, error) {
	requestURL, requestToken := os.Getenv(actionsTokenURLEnvVar), os.Getenv(actionsTokenTokenEnvVar)
	if requestURL == "" || requestToken == "" {
		return "", fmt.Errorf("no GitHub Actions OIDC token available: %s and %s are not set. iidp ci set-image runs only in GitHub Actions, in a job with permissions: id-token: write", actionsTokenURLEnvVar, actionsTokenTokenEnvVar)
	}
	u, err := url.Parse(requestURL)
	if err != nil {
		return "", fmt.Errorf("%s is not a URL: %w", actionsTokenURLEnvVar, err)
	}
	q := u.Query()
	q.Set("audience", audience)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "bearer "+requestToken)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting a GitHub Actions OIDC token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("requesting a GitHub Actions OIDC token: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var token struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &token); err != nil || token.Value == "" {
		return "", errors.New("requesting a GitHub Actions OIDC token: the response carried no token")
	}
	return token.Value, nil
}

// callDeployGate posts the deploy request to the gate. A gate that cannot
// be reached, or answers 502, 503 or 504 (restarting behind Traefik), is
// tried again twice; a deploy is safe to repeat, since the gate commits
// nothing when the Environment already runs the tag. Every other refusal
// is returned with the gate's own message.
func callDeployGate(ctx context.Context, client *http.Client, gate, token string, request deploygate.Request, retryDelay time.Duration) (deploygate.Response, error) {
	if retryDelay == 0 {
		retryDelay = 10 * time.Second
	}
	body, err := json.Marshal(request)
	if err != nil {
		return deploygate.Response{}, err
	}
	const attempts = 3
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return deploygate.Response{}, ctx.Err()
			case <-time.After(retryDelay):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, gate+deploygate.DeployPath, bytes.NewReader(body))
		if err != nil {
			return deploygate.Response{}, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("calling the Deploy gate at %s: %w", gate, err)
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
			var res deploygate.Response
			if err := json.Unmarshal(data, &res); err != nil {
				return deploygate.Response{}, fmt.Errorf("the Deploy gate at %s answered with something other than a deploy result: %w", gate, err)
			}
			return res, nil
		case resp.StatusCode == http.StatusBadGateway, resp.StatusCode == http.StatusServiceUnavailable, resp.StatusCode == http.StatusGatewayTimeout:
			lastErr = fmt.Errorf("the Deploy gate at %s is unavailable: HTTP %d: %s", gate, resp.StatusCode, gateMessage(data))
			continue
		default:
			return deploygate.Response{}, fmt.Errorf("the Deploy gate did not deploy %s (HTTP %d): %s", request.Application, resp.StatusCode, gateMessage(data))
		}
	}
	return deploygate.Response{}, lastErr
}

// gateMessage is the error message in a gate's refusal, or the raw body
// when it is not one (a proxy's own error page).
func gateMessage(data []byte) string {
	var refusal deploygate.ErrorResponse
	if err := json.Unmarshal(data, &refusal); err == nil && refusal.Error != "" {
		return refusal.Error
	}
	return strings.TrimSpace(string(data))
}

func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
