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
	"github.com/Itema-as/iidp/internal/templates"
)

// runWizard asks the nine questions docs/design.md's wizard describes, in
// order, with the agreed defaults, reading from cmd.InOrStdin() and writing
// to cmd.OutOrStdout() — the same seam the CLI's tests already inject
// (docs/implementation-notes/14-cli-wizard.md). A question whose flag was
// already given (cmd.Flags().Changed) is skipped entirely: it never prints
// anything. Answers are applied with Flags().Set, which both assigns opts'
// bound field and marks the flag Changed, so the rest of the command (plan,
// detectMigrationCommand) cannot tell an answer typed at a prompt from one
// given on the command line. loginCookieDomain reads the Itema login
// cookie's domain from platform.yaml; it is called only when question 7
// needs it.
func runWizard(cmd *cobra.Command, opts *createOptions, p *prompt.Prompter, loginCookieDomain func() (string, error)) error {
	f := cmd.Flags()
	out := cmd.OutOrStdout()

	// 1. Application name.
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

	// 2. Create or Adopt?
	if !f.Changed("path") {
		choice, err := p.Choice("Create or Adopt?", []string{pathCreate, pathAdopt}, pathCreate)
		if err != nil {
			return err
		}
		if err := f.Set("path", choice); err != nil {
			return err
		}
	}
	// 2b. Adopt asks for the repository to open a pull request on.
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

	// 3. Kind and framework. On the Create path the framework decides the
	// Kind, except "other", which asks for it directly. Adopt asks
	// neither: an existing Dockerfile's Kind cannot be derived at all (it
	// is required as a flag, checked once the repository is cloned), and
	// without one the framework is detected from the repository itself,
	// not chosen (docs/implementation-notes/15-cli-adopt-path.md).
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
		// The legacy bare path (no --path, kept for compatibility per
		// docs/implementation-notes/11-cli-create-path.md): the wizard
		// never chooses it itself (question 2 only offers Create or
		// Adopt), but a flag combination can still reach here, so the
		// question is asked all the same. Adopt (opts.path == pathAdopt)
		// asks nothing here, per the comment above.
		if err := askKind(f, p); err != nil {
			return err
		}
	}

	// 4. Postgres database, then, if enabled, the migration command.
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

	// 5. Staging Environment.
	if !f.Changed("staging") {
		yes, err := p.YesNo("Staging Environment?", false)
		if err != nil {
			return err
		}
		if err := f.Set("staging", strconv.FormatBool(yes)); err != nil {
			return err
		}
	}

	// 6. Custom domain.
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

	// 7. Itema login: docs/design.md's wizard offers this unless a custom
	// domain is outside the login cookie domain, which only platform.yaml
	// knows, so it is read (once, lazily) only when domains were given.
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
	// 7b. Sign-in groups, right after login, when login is on.
	if opts.login && !f.Changed("login-group") {
		if err := askSignInGroups(f, p, out); err != nil {
			return err
		}
	}

	// 8. Size.
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

// askKind asks the Kind question (used both for --framework other, which
// does not derive one, and for the legacy bare path) and sets --kind with
// the answer.
func askKind(f *pflag.FlagSet, p *prompt.Prompter) error {
	kind, err := p.Choice("Kind", []string{platformrepo.KindWebService, platformrepo.KindStaticSite}, platformrepo.KindWebService)
	if err != nil {
		return err
	}
	return f.Set("kind", kind)
}

// askMigrationCommand shows docs/design.md's migration help text, including
// the detected suggestion (or "nothing" when none was found), and asks for
// the command. The flag is set even on a blank answer, so
// detectMigrationCommand does not run detection a second time and
// potentially disagree with a developer who declined the suggestion.
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

// migrationHelpText is docs/design.md's "The wizard" help text for the
// migration command question, with det filled in.
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

// splitList turns a wizard answer into the values a repeatable flag
// (--domain, --login-group) would have received, one per repeated flag:
// comma-separated, trimmed, with blanks and a literal "none" (the
// question's bracketed default) dropped.
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

// askItemaLogin asks docs/design.md's wizard question 7, "Itema login?
// [no]", and sets --login with the answer. runWizard only calls it when
// every custom domain given is inside the login cookie domain (the cookie
// never reaches a host outside it): the wizard does not ask a question
// whose answer would then be refused
// (docs/implementation-notes/76-login-in-zone-domains.md).
func askItemaLogin(f *pflag.FlagSet, p *prompt.Prompter) error {
	yes, err := p.YesNo("Itema login?", false)
	if err != nil {
		return err
	}
	return f.Set("login", strconv.FormatBool(yes))
}

// askSignInGroups asks which Entra groups may sign in, saying how to find a
// group's object id, and sets --login-group once per id. The default,
// none, sets nothing: every Itema user gets in. An answer that is not a
// comma-separated list of GUIDs is asked again, with the reason
// (docs/implementation-notes/92-sign-in-groups.md).
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

// printSummary lists every choice the developer made — the wizard's
// question 9 — before asking for confirmation. preview is what
// platformrepo.Writer.PreviewApplication reports for app: real addresses
// and domain classification read from platform.yaml, without writing
// anything. migrationCommand is the command resolved for iidp.yaml.
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
