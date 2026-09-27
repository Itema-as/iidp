package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Itema-as/iidp/internal/deploygate"
	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
	"github.com/Itema-as/iidp/internal/platformstate"
)

// statusOptions are the flags of app status.
type statusOptions struct {
	json         bool
	platformRepo string
}

func newAppStatusCommand(deps Dependencies) *cobra.Command {
	var opts statusOptions
	cmd := &cobra.Command{
		Use:   "status <name>",
		Short: "Show the live state of every Environment of an Application",
		Long: "Show, for every Environment of an Application (Preview Environments\n" +
			"included): whether it is serving (Healthy, Degraded or Unknown), what is\n" +
			"changing (Arriving, Unreleased, Deploying, Updating or Leaving), and whether\n" +
			"that change is stuck and why; ArgoCD's sync and health and its last sync, the\n" +
			"image running and when it was deployed, the pods ready and their restarts,\n" +
			"the last migration, the last run of each Scheduled task, the addresses, and\n" +
			"links to ArgoCD and to the logs in Grafana Cloud. It shows no logs.\n\n" +
			"It asks the Deploy gate's service at https://deploy.<baseDomain> (baseDomain\n" +
			"from the Platform repository's platform.yaml), sending your gh auth token.\n" +
			"The service shows the status only to someone who can read the Application's\n" +
			"repository on GitHub. Nothing talks to Kubernetes from your machine.\n\n" +
			"--json prints the answer as JSON, in the shape README.md documents.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAppStatus(cmd, args[0], opts, deps)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&opts.json, "json", false, "Print the status as JSON")
	f.StringVar(&opts.platformRepo, "platform-repo", platform.RepositoryURL, "Git URL of the Platform repository")
	_ = f.MarkHidden("platform-repo")
	return cmd
}

func runAppStatus(cmd *cobra.Command, name string, opts statusOptions, deps Dependencies) error {
	if err := platformrepo.ValidateName(name); err != nil {
		return err
	}
	token, err := deps.TokenSource.Token()
	if err != nil {
		return fmt.Errorf("not logged in to GitHub, and the Deploy gate needs your GitHub token to show a status: %w", err)
	}

	// The gate's address follows platform.yaml's baseDomain. If the
	// Platform repository cannot be read, Itema's Platform is assumed: the
	// gate then says for itself whether the token may see anything.
	gate := platform.DefaultDeployGateURL
	writer := &platformrepo.Writer{URL: opts.platformRepo, Auth: git.Auth{Token: token}}
	if cfg, err := writer.ReadConfig(cmd.Context()); err == nil {
		gate = cfg.DeployGateURL()
	} else {
		fmt.Fprintf(cmd.ErrOrStderr(), "Could not read platform.yaml, so asking the Deploy gate at %s: %v\n", gate, err)
	}

	client := deps.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: time.Minute}
	}
	status, err := fetchStatus(cmd, client, gate, token, name)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if opts.json {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}
	printStatus(out, status)
	return nil
}

func fetchStatus(cmd *cobra.Command, client *http.Client, gate, token, name string) (platformstate.Status, error) {
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, gate+deploygate.StatusPath+url.PathEscape(name), nil)
	if err != nil {
		return platformstate.Status{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return platformstate.Status{}, fmt.Errorf("the Deploy gate at %s cannot be reached: %w", gate, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode == http.StatusOK {
		var status platformstate.Status
		if err := json.Unmarshal(data, &status); err != nil {
			return platformstate.Status{}, fmt.Errorf("the Deploy gate at %s answered with something other than a status: %w", gate, err)
		}
		return status, nil
	}
	var refusal deploygate.ErrorResponse
	if json.Unmarshal(data, &refusal) != nil || refusal.Error == "" {
		// Not the gate's own answer: a proxy's error page, or a gate from
		// before iidp app status.
		if resp.StatusCode >= 500 {
			return platformstate.Status{}, fmt.Errorf("the Deploy gate at %s is unavailable: HTTP %d: %s", gate, resp.StatusCode, strings.TrimSpace(string(data)))
		}
		return platformstate.Status{}, fmt.Errorf("the Deploy gate at %s does not answer status calls (HTTP %d); it may be older than iidp app status", gate, resp.StatusCode)
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return platformstate.Status{}, fmt.Errorf("unknown Application %s: %s", name, refusal.Error)
	case http.StatusForbidden:
		return platformstate.Status{}, fmt.Errorf("no access to %s's status: %s", name, refusal.Error)
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return platformstate.Status{}, fmt.Errorf("the Deploy gate at %s is unavailable: HTTP %d: %s", gate, resp.StatusCode, refusal.Error)
	default:
		return platformstate.Status{}, fmt.Errorf("the Deploy gate did not show %s's status (HTTP %d): %s", name, resp.StatusCode, refusal.Error)
	}
}

// printStatus prints one block per Environment.
func printStatus(out io.Writer, status platformstate.Status) {
	fmt.Fprintf(out, "%s (%s)\n", status.Application, status.Repository)
	if len(status.Environments) == 0 {
		fmt.Fprintln(out, "\nNo Environments.")
	}
	for _, env := range status.Environments {
		fmt.Fprintf(out, "\n%s\n", env.Name)
		// line prints one field; an empty label continues the one above.
		line := func(label, text string) {
			if label != "" {
				label += ":"
			}
			fmt.Fprintf(out, "  %-11s%s\n", label, text)
		}
		if env.ArgoCD == nil {
			line("Status", "not on the Platform yet: ArgoCD picks a new Environment up within a few minutes")
			continue
		}
		// A Deploy gate older than Condition and Activity sends neither.
		if c := env.Condition; c != nil {
			text := c.State
			if c.Reason != "" {
				text += ": " + c.Reason
			}
			line("Condition", text)
		}
		if env.Activity != nil {
			line("Activity", activityText(*env.Activity))
		}
		state := env.ArgoCD.Sync + ", " + env.ArgoCD.Health
		if op := env.ArgoCD.Operation; op != nil {
			state += ", last sync " + op.Phase
			if op.FinishedAt != nil {
				state += " " + statusTime(*op.FinishedAt)
			} else if op.StartedAt != nil {
				state += ", started " + statusTime(*op.StartedAt)
			}
			if op.Phase != "Succeeded" && op.Phase != "Running" && op.Message != "" {
				state += ": " + op.Message
			}
		}
		line("Status", state)
		if env.Image == nil {
			line("Image", "none yet: nothing has been deployed")
		} else {
			image := env.Image.Repository + ":" + env.Image.Tag
			if env.Image.DeployedAt != nil {
				image += ", deployed " + statusTime(*env.Image.DeployedAt)
			}
			line("Image", image)
			restarts := "restarts"
			if env.Pods.Restarts == 1 {
				restarts = "restart"
			}
			line("Pods", fmt.Sprintf("%d/%d ready, %d %s", env.Pods.Ready, env.Pods.Total, env.Pods.Restarts, restarts))
		}
		if env.Migration != nil {
			line("Migration", "last run "+runText(*env.Migration))
		}
		label := "Tasks"
		for _, task := range env.Tasks {
			text := task.Name + " (" + task.Schedule + "): no run yet"
			if task.LastRun != nil {
				text = task.Name + " (" + task.Schedule + "): last run " + runText(*task.LastRun)
			}
			line(label, text)
			label = ""
		}
		label = "Addresses"
		for _, address := range env.Addresses {
			line(label, address)
			label = ""
		}
		if env.Links != nil && env.Links.ArgoCD != "" {
			line("ArgoCD", env.Links.ArgoCD)
		}
		if env.Links != nil && env.Links.Grafana != "" {
			line("Logs", env.Links.Grafana)
		}
	}
}

// hopText is a Deploy's hop in words.
var hopText = map[string]string{
	platformstate.HopAccepted:         "accepted",
	platformstate.HopWaitingForArgoCD: "waiting for ArgoCD",
	platformstate.HopApplying:         "applying",
	platformstate.HopRollingOut:       "rolling out",
	platformstate.HopServing:          "serving",
}

// activityText is an Activity in words: what is changing, the Deploy's
// tag and hop, and why it is stuck.
func activityText(a platformstate.Activity) string {
	text := a.State
	hop := ""
	if d := a.Deploy; d != nil {
		text += " " + d.Tag
		if d.Promote {
			text += " (Promote)"
		}
		hop = hopText[d.Hop]
		if hop == "" {
			hop = d.Hop
		}
	}
	switch {
	case a.Stuck && hop != "":
		// The hop's name, as the five hops are named: "Rolling out".
		text += ", stuck at " + strings.ToUpper(hop[:1]) + hop[1:]
	case a.Stuck:
		text += ", stuck"
	case hop != "":
		text += ", " + hop
	}
	if a.Stuck && a.Reason != "" {
		text += ": " + a.Reason
	}
	return text
}

func runText(run platformstate.Run) string {
	switch {
	case run.FinishedAt != nil:
		return run.Result + " " + statusTime(*run.FinishedAt)
	case run.StartedAt != nil:
		return run.Result + ", started " + statusTime(*run.StartedAt)
	default:
		return run.Result
	}
}

// statusTime is a time as iidp app status prints it, in UTC, which reads
// the same on every machine.
func statusTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04 UTC")
}
