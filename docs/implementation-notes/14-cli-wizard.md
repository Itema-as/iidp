# #14 CLI: interactive wizard

Questions that came up while adding the interactive wizard to `iidp app create`, and the answer chosen for each. `docs/design.md`'s "The wizard" section (the nine questions, their order, defaults and migration help text) was the source of truth.

## TTY detection: `golang.org/x/term.IsTerminal`, not `os.Stdin.Stat()`'s `ModeCharDevice` bit

The ticket offered either. `os.ModeCharDevice` was tried first, since it needs no dependency: `os.Stdin.Stat()` on a real terminal reports it. But `/dev/null` is itself a character device (`crw-rw-rw-`), so that bit is set for `/dev/null` exactly as it is for a pty — it cannot tell a real terminal from stdin redirected from `/dev/null`, which every non-interactive script, cron job and, on the machine this ticket was implemented on, a plain `go test` run all do. Confirmed by writing a two-line program and running it both attached to a terminal and with `< /dev/null`: both reported `chardev true`. Shipping the naive check would have made `iidp app create --name x --kind web-service` (every flag given, no terminal) launch the wizard whenever stdin happened to be `/dev/null` instead of a closed pipe — a regression the ticket's own "without a TTY ... behaviour stays as today" line rules out.

Chosen: `golang.org/x/term.IsTerminal(int(os.Stdin.Fd()))`, which asks the kernel for the terminal's `termios` (an `ioctl`) rather than inspecting the file's mode bits, and correctly reports `false` for `/dev/null`. This is the one new dependency the ticket allowed for exactly this reason. `internal/cli/app.go`'s `isInteractive` checks the hidden `--interactive` flag first, so tests never depend on this detection at all.

## The hidden `--interactive` flag, not `Dependencies.Interactive`

The ticket offered either. A hidden bool flag follows the precedent `--platform-repo` and `--app-dir` already set (`docs/implementation-notes/10-cli-platform-repository.md`, `13-cli-capabilities.md`): a real, if unadvertised, way to force the behaviour, exercised through the same `cli.Run`/`cli.RunWith` seam as every other flag, with no change to the `Dependencies` struct other tickets (#12, #17) are also editing in parallel.

## `runAppCreate` takes `*createOptions`, not `createOptions`

The wizard answers questions by calling `cmd.Flags().Set(name, value)`, which both assigns the flag's bound variable and marks it `Changed` — precisely what lets the rest of the command (`plan`, `detectMigrationCommand`) treat a wizard answer exactly like a flag given on the command line, with no parallel "answers" struct to keep in sync. But `newAppCreateCommand`'s `RunE` closure previously passed its local `opts` to `runAppCreate` **by value**: flags are bound to the closure's `opts` (via `StringVar(&opts.name, ...)` and so on), so a `Set` call after that copy was taken would mutate the closure's copy while `runAppCreate` kept working from its own, now-stale one. Caught by the wizard's first test: after answering "create" to "Create or Adopt?", the very next question was "Kind" instead of "Framework" — `opts.path` inside `runWizard` was still `""`. Fixed by changing the call to `runAppCreate(cmd, &opts, deps)` and `runAppCreate`'s signature to `*createOptions`; `runWizard` already took a pointer.

## One `prompt.Prompter` for the whole command

`prompt.New` wraps its reader in a `bufio.Reader`, which reads ahead of what individual calls return. The wizard's questions and the closing "Proceed?" confirmation were originally two separate `prompt.New` calls over the same `cmd.InOrStdin()`; against a short, fully-scripted test input, the first `bufio.Reader` buffered the *entire* remaining input (including the answer meant for "Proceed?") in one read from the underlying `strings.Reader`, then returned only the lines the wizard asked for. A second `bufio.Reader` created afterwards saw the underlying reader already at EOF and never saw the buffered-but-unread bytes, producing "no more input" on the confirmation. Fixed by constructing one `*prompt.Prompter` in `runAppCreate` and passing it into `runWizard`, reusing it for the confirmation too.

## No third-party TUI library

`internal/prompt` is three functions (`Text`, `YesNo`, `Choice`) over a `bufio.Reader`/`io.Writer`, each re-asking on invalid input. A survey/TUI library (`survey`, `bubbletea`) would take over the terminal (raw mode, its own event loop), which is incompatible with the CLI's testing seam: the ticket's tests script answers as plain lines through the same `stdin` reader `cli.Run` already accepts, and assert on plain text written to `stdout`. That rules out anything that assumes a real terminal.

## Which flags the wizard asks about

Only the nine questions `docs/design.md`'s "The wizard" section lists in order: name, Create-or-Adopt, Kind/framework, Postgres (+ migration command), staging, custom domain, Itema login (not yet askable, see below), size, then the summary and confirmation. `--owner`, `--private`/`--public`, `--image`, `--port` and `--probe-path` are not design.md wizard questions and keep their flag defaults silently, exactly as today; a developer who wants non-default values for those still passes them as flags.

## The custom domain question accepts a comma-separated list

`docs/design.md`'s question 6 text is singular ("Custom domain? [none]"), but `--domain` is already repeatable on the command line (`--domain a --domain b`, since #13). Asking the question once per possible domain would need to know in advance how many the developer wants; chosen instead: one prompt, comma-separated for more than one (`splitDomains` in `internal/cli/wizard.go`, dropping blanks and a literal "none", the bracketed default), calling `f.Set("domain", host)` once per host so the rest of the command sees exactly what repeating `--domain` would have produced. This is an extension of the question, not a deviation from it: a developer with one domain types it exactly as design.md shows.

## Question 2 only ever resolves to Create or Adopt, never the legacy bare path

Before this ticket, omitting `--path` entirely wrote only the Platform repository and created no Application repository (`docs/implementation-notes/11-cli-create-path.md`'s "deliberate compatibility" shim, kept only so old scripts and flag-only tests keep working). `docs/design.md`'s wizard question 2 is literally "Create or Adopt?" — it has no third option — so the wizard's choice prompt only ever offers `create`/`adopt`, defaulting to `create` (Adopt is not built yet; #15). A wizard run therefore always ends up on the Create path (or the Adopt refusal below), never the bare one; the bare path remains reachable only by giving `--kind` (and no `--path`) as a flag, interactively or not.

## Choosing Adopt stops the wizard immediately

Once the developer answers "adopt" to question 2, nothing else is asked: `opts.plan` already refuses `--path adopt` with "not implemented yet; see issue #15", so asking Postgres, staging, a domain and a size first would waste the developer's time on a command that is going to fail regardless. `runWizard` prints "Adopt is not available yet (see issue #15)." and returns immediately; `plan` then produces its pre-existing error and exit code.

## The Kind/framework question's Adopt hook

The ticket's acceptance criterion ("skipped when Adopt finds a Dockerfile") cannot be implemented today: Adopt (#15) does not exist, so there is no target repository to detect a Dockerfile in. The block in `runWizard` that asks Framework (and, for `other`, Kind) carries a `TODO(#15)` comment saying exactly what #15 needs to add: skip the block entirely when Adopt finds an existing Dockerfile, since that Dockerfile is deployed as-is and its Kind is whatever the developer already chose when the repository was created (`docs/design.md`: "an existing Dockerfile is never touched").

## The Itema login question's #18 hook

Same shape of problem: `docs/design.md`'s question 7 is gated on "no custom domain given", but there is no `--login` flag on `origin/main` to wire it to (issue #18, blocked on #8 and #4 in addition to this ticket, has not landed). `itemaLoginQuestion` in `internal/cli/wizard.go` is a clearly named, currently-empty function called at the point in the question order question 7 belongs, with a `TODO(#18)` comment naming the exact behaviour to add once the flag exists: ask "Itema login? [no]" when `len(opts.domains) == 0`, refuse it outright otherwise (the same way `--postgres` on a Static site is refused today), and `f.Set("login", ...)` with the answer so it flows through `plan` like every other question.

## The migration command is always `f.Set`, even when blank

`detectMigrationCommand` (the function that resolves the final `postgres.migrationCommand` after the wizard, unchanged in shape since #13) used to treat an empty `--migration-command` as "not given" and re-run detection. That is wrong for the wizard: if a developer is shown a detected suggestion and deliberately answers blank (declining it), re-running detection afterwards would print the same suggestion again and silently use it, overriding the developer's explicit "no". Fixed by adding `migrationCommandSet` to `createPlan` (`cmd.Flags().Changed("migration-command")`, computed once in `plan`) and checking that instead of `migrationCommand != ""`; the wizard's `askMigrationCommand` always calls `f.Set("migration-command", answer)`, blank or not, so the flag is `Changed` either way and detection never runs a second time. `resolveMigrationDir` was extracted out of the old `detectMigrationCommand` so the wizard's `suggestMigrationCommand` (used only to compute the prompt's default and help text, printing nothing) and the real `detectMigrationCommand` (used once, after the wizard, to resolve the final value and print what it found) share the exact same directory-resolution order instead of two copies that could drift.

## `platformrepo.Writer.PreviewApplication`: the summary's source of truth, with no lasting side effect

The summary must show real Platform addresses, ArgoCD/Grafana links and domain classification (which needs `wildcard`/`cloudflareZone` from `platform.yaml`) *before* asking for confirmation, but declining must leave "nothing cloned, nothing created". These pull in different directions: showing real data needs to read `platform.yaml`, which needs a clone.

Chosen: interpret "no side effects" as no commit pushed to the Platform repository and no GitHub repository created — not "no local temporary directory ever created". `Writer.attemptCreate` (already the one function `CreateApplication` used, shared with the retry logic) gained a `preview bool` parameter: true stops right after validation (the clone, `LoadConfig`, `checkApplicationAbsent`, the Postgres bucket/endpoint check, `ValidateDomains`), before `writeEnvironment` ever runs and before `Add`/`Commit`/`Push`, computing the `Files` list directly instead (the paths are deterministic from the Application name and which Environments exist). `Writer.PreviewApplication` calls it with `preview: true`; `CreateApplication`'s retry loop is unaffected (still `preview: false`). The temporary clone directory `attemptCreate` always uses is removed by its pre-existing `defer os.RemoveAll(dir)` regardless of which path was taken, so nothing outlives the call either way. This costs one extra clone of the Platform repository per interactive run (skipped entirely with `--yes`, which still skips the confirmation exactly as before this ticket), which is the same cost `CheckAvailable` already pays for the Create path today.

## `GET /user` (for `--owner user`) also runs before the confirmation, on purpose

The same "no side effects on decline" question applies to one more call: with `--owner user`, resolving the developer's personal login (`ghClient.CurrentUser`) happens before the summary is built, because the summary shows that resolved login as the Application repository's owner. `GET /user` reads nothing but the token's own identity and creates nothing — it is the read-only counterpart of `PreviewApplication`'s clone, justified the same way: showing the real owner in the summary is worth one read-only API call, and no GitHub repository, commit or push happens unless the developer confirms.

## The migration help text's "Detected:" line names the tool, not just the path

`docs/design.md`'s literal example is `Detected: prisma/schema.prisma → suggested "npx prisma migrate deploy"` — a bare file path. `migrationHelpText` (`internal/cli/wizard.go`) prints `Detected: Prisma (prisma/schema.prisma) → suggested "npx prisma migrate deploy"` instead, using `migrate.Detection.Tool` as `internal/migrate` already formats it (`"Prisma (prisma/schema.prisma)"`, `"Drizzle (drizzle.config.ts)"`, `"an npm migrate script"`) rather than adding a second field to `Detection` just to recover the bare path. `Detection.Tool`'s format is also what `detectMigrationCommand`'s own "Detected %s; migration command: %s" message already prints and what `TestAppCreateDetectsMigrationTooling` already asserts on (#13); reusing it keeps one source of truth for how a detection is named instead of two slightly different renderings of the same `Detection`. The substance design.md asks for — the file that triggered detection and the suggested command — is present either way.

## `runAppCreate` stays one function

Issue #11's notes already settled this for the Create-then-write sequence this ticket extends: "`internal/cli/app.go`'s `runAppCreate` stays one function covering the whole Create-then-write sequence; it is the ticket's documented orchestration seam, and splitting it did not make any single piece easier to test given the seam is already the whole command." The wizard adds two more sequential steps (asking questions, then a preview-and-confirm) to the same seam rather than opening a second one; each step is a self-contained block with its own comment, and the tests exercise the seam exactly as before (`cli.RunWith`), not any of the internal steps individually.

## `--yes` skips the summary print entirely, not just the final question

The ticket says "`--yes` skips the confirmation as today". The non-interactive path already prints its own inline "Creating Application..." / "Writing the prod Environment..." progress messages, which cover the same ground as the wizard's summary; printing both would be redundant. Chosen: the summary and the confirmation are one guarded block (`if interactive && !opts.yes`), so `--yes` (interactive or not) goes straight from planning to writing, exactly as a fully flag-driven, non-interactive run always has.

## The summary's `visibility`, `Path`, and other free-text fields are not test-golden-file exact

The ticket asks for a summary "listing every choice"; it does not prescribe exact wording. `printSummary` (`internal/cli/wizard.go`) mirrors the shape of the pre-existing "Creating Application..." / "Writing the prod Environment..." progress printing rather than introducing a third format, and the tests assert on the presence of each labelled value (`"Kind:       web-service"`, `"Postgres:   enabled"`, and so on) rather than the summary's byte-for-byte layout, consistent with the testing decisions in `docs/design.md` ("observable output", not exact formatting).

## Testing

Driven entirely through `cli.Run`/`cli.RunWith` with `--interactive` and a scripted `stdin` reader, against the same bare Platform repository and fake GitHub server fixtures the flag-driven tests (`internal/cli/app_create_test.go`, `app_create_create_test.go`, `app_create_capabilities_test.go`) already use — the ticket's agreed seam, and the same one every other CLI ticket in this repository has used. New file: `internal/cli/app_create_wizard_test.go`. `internal/prompt` gets no `_test.go` of its own, matching the layout `apprepo`/`templates`/`migrate` already established (#11, #13's notes): its only caller is the wizard, and the wizard is what the ticket's seam names.
