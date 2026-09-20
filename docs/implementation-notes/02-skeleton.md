# #2 Go module, iidp version, CI and release skeleton

Questions that came up while building the skeleton, and the answer chosen for each.

## Command tree: cobra or the standard library?

Options: `flag` from the standard library with a hand-written subcommand switch, or `github.com/spf13/cobra`.

Chosen: cobra. Phase 1 needs nested commands (`app create`, `app add-capability`, `app delete`, `secret set`), a flag for every wizard question, and generated help. cobra gives all of that and is the one dependency the spec allows beyond the standard library. It is used only in `internal/cli`; the test seam (`cli.Run(args, stdin, stdout, stderr) int`) does not expose it.

## Where does the injected version live, and what does `iidp version` print?

Options for the ldflags target: `main.version` in `cmd/iidp`, a variable in `internal/cli`, or a dedicated `internal/version` package.

Chosen: `internal/version.Version`, default `"dev"`. `main` stays a one-liner that calls `cli.Run`, and the tests set the variable the same way ldflags do. `iidp version` prints the bare version string followed by a newline (`0.3.1`, or `dev`), which is the most useful form for scripts and matches what the tag carries; no `iidp` prefix, no commit or build date.

## Where do the compiled-in org and Platform repository name live?

Options: constants in the future `internal/platformrepo` package, or a package of their own.

Chosen: `internal/platform` with `Org`, `RepositoryName` and the derived `Repository` (`owner/name`). It is the single place these may appear in code; `platformrepo` will grow git logic and should import the values rather than own them. The root command's long help uses `platform.Repository`, so the package is exercised from the start.

## Homebrew: formula or cask?

The ticket says "formula". GoReleaser 2.18 marks the formula-based `brews` section as deprecated and `goreleaser check` fails on deprecated options, which would break CI.

Chosen: `homebrew_casks`, the replacement GoReleaser generates for plain binaries, writing `Casks/iidp.rb` in `Itema-as/homebrew-tap`. Install is `brew install --cask Itema-as/tap/iidp`. Casks are macOS only, so Linux users install from GitHub Releases; the binaries are not signed, so the cask clears the quarantine bit in a post-install hook. If the tap must also serve Homebrew on Linux, a formula can be added back at the cost of the deprecation warning.

## Which GitHub repository does the release go to?

Options: hardcode `Itema-as/iidp` in `release.github`, or leave it out and let GoReleaser read owner and name from the git remote.

Chosen: read from the remote. The repository lives under a personal account today and moves to `Itema-as`; nothing but the compiled-in constant may hardcode an owner, and this way tags release correctly both before and after the move. The cask's `homepage` is templated from the same remote (`{{ trimsuffix .GitURL ".git" }}`) rather than written out. The tap repository (`Itema-as/homebrew-tap`) is the one exception, since the spec defines the tap as living under the org.

## Name of the tap token secret

Chosen: `HOMEBREW_TAP_GITHUB_TOKEN`, an Actions secret holding a fine-grained token with Contents read and write on `Itema-as/homebrew-tap` only. Documented under "Releasing" in the README. No secret was created.

## What runs in CI beyond `go vet` and `go test`?

Chosen: a `gofmt -l` check (the gate required before every commit, so CI enforces what contributors are asked to do by hand) in the Go job, and a separate job running `goreleaser check` so a broken release configuration fails a pull request instead of the tag. Both are cheap and there is no other place a release-config error would surface before a release. GoReleaser's `before.hooks: go mod tidy` was left out on purpose: a release should build exactly what is committed, and a tidy that changes `go.mod` would abort the release as a dirty tree anyway.

## Licence

The repository has no `LICENSE` file, so the cask carries no `license` stanza. Add one to both when the licence is decided.
