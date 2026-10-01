package cli

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Itema-as/iidp/internal/appconfig"
	"github.com/Itema-as/iidp/internal/apprepo"
	"github.com/Itema-as/iidp/internal/migrate"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
	"github.com/Itema-as/iidp/internal/prompt"
	"github.com/Itema-as/iidp/internal/render"
	"github.com/Itema-as/iidp/internal/templates"
)

// runWizard asks app create's questions, skipping any whose flag was given.
// Answers are applied with Flags().Set, which also marks the flag Changed,
// so the rest of the command cannot tell a typed answer from a flag.
// loginCookieDomain is called only when custom domains were given.
func runWizard(cmd *cobra.Command, opts *createOptions, p *prompt.Prompter, loginCookieDomain func() (string, error)) error {
	f := cmd.Flags()
	out := cmd.OutOrStdout()

	if !f.Changed("name") {
		name, err := p.Text("Application name", "", func(s string) error {
			return platformrepo.ValidateNewName(s)
		})
		if err != nil {
			return err
		}
		if err := f.Set("name", name); err != nil {
			return err
		}
	}

	if !f.Changed("path") {
		choice, err := p.Choice("Create or Adopt?", []string{pathCreate, pathAdopt}, pathCreate)
		if err != nil {
			return err
		}
		if err := f.Set("path", choice); err != nil {
			return err
		}
	}
	if opts.path == pathAdopt && !f.Changed("repo") {
		repo, err := p.Text("Application repository ("+platform.Org+"/name or a URL)", "", func(s string) error {
			if s == "" {
				return errors.New("a repository is required")
			}
			owner, name, err := parseRepoFlag(s)
			if err != nil {
				return err
			}
			return apprepo.CheckInOrg(owner, name)
		})
		if err != nil {
			return err
		}
		if err := f.Set("repo", repo); err != nil {
			return err
		}
	}

	// Adopt asks neither Kind nor framework: both come from what the
	// repository contains, checked once it is cloned.
	if opts.path == pathCreate {
		if !f.Changed("framework") {
			fw, err := p.Choice("Framework", []string{
				string(templates.NextJS), string(templates.ViteReact), string(templates.Other),
			}, string(templates.NextJS))
			if err != nil {
				return err
			}
			if err := f.Set("framework", fw); err != nil {
				return err
			}
		}
		if opts.framework == string(templates.Other) && !f.Changed("kind") {
			if err := askKind(f, p); err != nil {
				return err
			}
		}
	} else if opts.path == "" && !f.Changed("kind") {
		// The wizard never offers writing only the Platform repository, but
		// an explicit --path "" still reaches here.
		if err := askKind(f, p); err != nil {
			return err
		}
	}

	if !f.Changed("postgres") {
		yes, err := p.YesNo("Postgres database?", false)
		if err != nil {
			return err
		}
		if err := f.Set("postgres", strconv.FormatBool(yes)); err != nil {
			return err
		}
	}
	if opts.postgres && !f.Changed("migration-command") {
		if err := askMigrationCommand(cmd, opts, p, out); err != nil {
			return err
		}
	}

	if !f.Changed("staging") {
		yes, err := p.YesNo("Staging Environment?", false)
		if err != nil {
			return err
		}
		if err := f.Set("staging", strconv.FormatBool(yes)); err != nil {
			return err
		}
	}

	// Asked only with staging and an Application repository: a Preview
	// Environment uses staging's values and secrets and follows the
	// repository's pull requests, so without them a yes would only be
	// refused.
	if opts.staging && (opts.path == pathCreate || opts.path == pathAdopt) && !f.Changed("previews") {
		fmt.Fprintln(out, "Preview Environments (optional)")
		fmt.Fprintln(out, "Every open pull request labelled "+render.PreviewLabel+" gets an Environment of its own at")
		fmt.Fprintln(out, "<name>-pr-<number>.<baseDomain>, with staging's values and secrets, Itema login and an")
		fmt.Fprintln(out, "empty database, removed when the pull request closes or loses the label.")
		yes, err := p.YesNo("Preview Environments?", false)
		if err != nil {
			return err
		}
		if err := f.Set("previews", strconv.FormatBool(yes)); err != nil {
			return err
		}
	}

	if !f.Changed("domain") {
		answer, err := p.Text("Custom domain (comma-separated for more than one)", "none", nil)
		if err != nil {
			return err
		}
		for _, host := range splitList(answer) {
			if err := f.Set("domain", host); err != nil {
				return err
			}
		}
	}

	// Login is not offered when a custom domain is outside the login cookie
	// domain, which only platform.yaml knows.
	if !f.Changed("login") {
		offer := true
		if len(opts.domains) > 0 {
			cookieDomain, err := loginCookieDomain()
			if err != nil {
				return err
			}
			if outside := platformrepo.HostsOutsideLoginCookieDomain(opts.domains, cookieDomain); len(outside) > 0 {
				fmt.Fprintf(out, "Itema login is not offered: its sign-in cookie is set for %s, and %s outside it.\n", cookieDomain, strings.Join(outside, ", "))
				offer = false
			}
		}
		if offer {
			if err := askItemaLogin(f, p); err != nil {
				return err
			}
		}
	}
	if opts.login && !f.Changed("login-group") {
		if err := askSignInGroups(f, p, out); err != nil {
			return err
		}
	}

	if !f.Changed("size") {
		size, err := p.Choice("Size", sizes, "small")
		if err != nil {
			return err
		}
		if err := f.Set("size", size); err != nil {
			return err
		}
	}

	return nil
}

// askKind asks for the Kind and sets --kind.
func askKind(f *pflag.FlagSet, p *prompt.Prompter) error {
	kind, err := p.Choice("Kind", []string{platformrepo.KindWebService, platformrepo.KindStaticSite}, platformrepo.KindWebService)
	if err != nil {
		return err
	}
	return f.Set("kind", kind)
}

// askMigrationCommand asks for the migration command, suggesting the
// detected one. The flag is set even on a blank answer, so detection does
// not run again and override a developer who declined the suggestion.
func askMigrationCommand(cmd *cobra.Command, opts *createOptions, p *prompt.Prompter, out io.Writer) error {
	planSoFar, err := opts.plan(cmd)
	if err != nil {
		return err
	}
	det, ok, err := suggestMigrationCommand(planSoFar)
	if err != nil {
		return err
	}
	fmt.Fprint(out, migrationHelpText(det, ok))
	suggestion := ""
	if ok {
		suggestion = det.Command
	}
	answer, err := p.Text("Migration command", suggestion, nil)
	if err != nil {
		return err
	}
	return cmd.Flags().Set("migration-command", answer)
}

// migrationHelpText is the help text for the migration command question.
func migrationHelpText(det migrate.Detection, ok bool) string {
	var b strings.Builder
	b.WriteString("Migration command (optional)\n")
	b.WriteString("Runs once before every rollout, in a one-off container built from your\n")
	b.WriteString("image, with DATABASE_URL set. Leave empty if your app has no migrations.\n")
	if ok {
		fmt.Fprintf(&b, "Detected: %s → suggested %q\n", det.Tool, det.Command)
	} else {
		b.WriteString("Detected: nothing\n")
	}
	return b.String()
}

// splitList splits a comma-separated wizard answer into values for a
// repeatable flag, dropping blanks and the default "none".
func splitList(answer string) []string {
	var hosts []string
	for _, part := range strings.Split(answer, ",") {
		host := strings.TrimSpace(part)
		if host == "" || strings.EqualFold(host, "none") {
			continue
		}
		hosts = append(hosts, host)
	}
	return hosts
}

// askItemaLogin asks whether to require Itema login and sets --login.
func askItemaLogin(f *pflag.FlagSet, p *prompt.Prompter) error {
	yes, err := p.YesNo("Itema login?", false)
	if err != nil {
		return err
	}
	return f.Set("login", strconv.FormatBool(yes))
}

// askSignInGroups asks which Entra groups may sign in and sets --login-group
// once per id. The default, none, lets every Itema user in.
func askSignInGroups(f *pflag.FlagSet, p *prompt.Prompter, out io.Writer) error {
	fmt.Fprintln(out, "Sign-in groups (optional)")
	fmt.Fprintln(out, "Only members of these Entra groups get past Itema login; leave empty to let")
	fmt.Fprintln(out, "every Itema user in. Give each group's object id, comma-separated: "+platformrepo.FindGroupIDHelp+".")
	answer, err := p.Text("Sign-in groups", "none", func(s string) error {
		_, err := platformrepo.NormalizeLoginGroups(splitList(s))
		return err
	})
	if err != nil {
		return err
	}
	for _, id := range splitList(answer) {
		if err := f.Set("login-group", id); err != nil {
			return err
		}
	}
	return nil
}

// printSummary lists every choice before the confirmation. preview is
// Writer.PreviewApplication's result for app, with the real addresses.
func printSummary(out io.Writer, plan createPlan, app platformrepo.Application, migrationCommand string, preview platformrepo.Result, adoptFiles []string) {
	fmt.Fprintln(out, "\nSummary:")
	fmt.Fprintf(out, "  Name:       %s\n", plan.name)
	switch plan.path {
	case pathCreate:
		visibility := "private"
		if !plan.private {
			visibility = "public"
		}
		fmt.Fprintf(out, "  Path:       Create\n")
		fmt.Fprintf(out, "  Owner:      %s\n", platform.Org)
		fmt.Fprintf(out, "  Framework:  %s\n", plan.framework)
		fmt.Fprintf(out, "  Visibility: %s\n", visibility)
	case pathAdopt:
		fmt.Fprintf(out, "  Path:       Adopt\n")
		fmt.Fprintf(out, "  Repository: %s/%s\n", plan.repoOwner, plan.repoName)
		if len(adoptFiles) == 0 {
			fmt.Fprintln(out, "  Pull request adds: nothing (a Dockerfile, a deploy workflow and iidp.yaml already exist)")
		} else {
			fmt.Fprintln(out, "  Pull request adds:")
			for _, f := range adoptFiles {
				fmt.Fprintf(out, "    %s\n", f)
			}
		}
	default:
		fmt.Fprintf(out, "  Path:       writes only %s\n", platform.Repository)
	}
	fmt.Fprintf(out, "  Kind:       %s\n", app.Kind)
	fmt.Fprintf(out, "  Size:       %s\n", app.Size)
	fmt.Fprintf(out, "  Port:       %d\n", app.Port)
	fmt.Fprintf(out, "  Probe path: %s\n", app.ProbePath)
	if app.Postgres {
		fmt.Fprintf(out, "  Postgres:   enabled (migration command for %s: %q)\n", appconfig.FileName, migrationCommand)
	} else {
		fmt.Fprintln(out, "  Postgres:   disabled")
	}
	if app.Staging {
		fmt.Fprintln(out, "  Staging:    enabled, its own address and database")
	} else {
		fmt.Fprintln(out, "  Staging:    disabled")
	}
	if app.Previews {
		fmt.Fprintf(out, "  Previews:   enabled, for pull requests labelled %s (%s)\n", render.PreviewLabel, preview.PreviewAddress)
	} else {
		fmt.Fprintln(out, "  Previews:   disabled")
	}
	if app.Login {
		fmt.Fprintln(out, "  Login:      Itema (Entra ID) sign-in required")
		fmt.Fprintf(out, "  Sign-in groups: %s\n", signInGroupsText(app.LoginGroups))
	} else {
		fmt.Fprintln(out, "  Login:      disabled")
	}
	fmt.Fprintf(out, "  Address:    %s\n", preview.Address)
	if preview.StagingAddress != "" {
		fmt.Fprintf(out, "  Staging address: %s\n", preview.StagingAddress)
	}
	if len(preview.Domains) == 0 {
		fmt.Fprintln(out, "  Domains:    none")
	} else {
		fmt.Fprintln(out, "  Domains:")
		for _, d := range preview.Domains {
			switch {
			case d.Wildcard:
				fmt.Fprintf(out, "    %s (wildcard certificate, DNS automatic)\n", d.Host)
			case d.Automated:
				fmt.Fprintf(out, "    %s (DNS automatic)\n", d.Host)
			default:
				fmt.Fprintf(out, "    %s (needs a CNAME)\n", d.Host)
			}
		}
	}
	if preview.Config.ArgoCDURL != "" {
		fmt.Fprintf(out, "  ArgoCD:     %s\n", preview.Config.ArgoCDURL)
	}
	if preview.Config.GrafanaURL != "" {
		fmt.Fprintf(out, "  Grafana:    %s\n", preview.Config.GrafanaURL)
	}
}
