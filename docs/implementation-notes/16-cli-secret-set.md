# #16 CLI: secret set

Questions that came up while building `iidp secret set`, and the answer chosen for each. Sources were checked on 2026-09-21.

## The sops Go library pulls in AWS, GCP, Azure and HashiCorp Vault

**Question.** The ticket recommends `github.com/getsops/sops/v3` (`sops/v3`, `sops/v3/aes`, `sops/v3/age`, `sops/v3/keyservice`, `sops/v3/stores/yaml`), with a fallback to shelling out to a `sops` binary if the library "turns out unusable (build size, API churn)".

**What was tried.** A scratch module importing only `github.com/getsops/sops/v3/age` and `github.com/getsops/sops/v3/aes` (the two narrowest packages the ticket names) was `go mod tidy`'d and built. The result: 332 modules in the build list and, imported into a two-line `main`, the AWS SDK v2 (S3, credentials, IMDS), Google Cloud SDK (storage, monitoring, logging, OpenTelemetry exporters), the Azure SDK, HashiCorp Vault's API client, `ory/dockertest`, `mongo-driver`, and more; `go build` of that trivial program produced a 49 MB binary. `go list -deps` confirmed these are real imports, not just go.sum bookkeeping: `sops/v3/age` imports the core `sops` package, which references every master-key backend (KMS, GCP KMS, Azure Key Vault, HashiCorp Vault, PGP) from the same files that implement age support, so there is no partial import that avoids them. This matches the repository's own experience elsewhere: today's entire dependency graph is `cobra` and `yaml.v3` (4 modules total, indirect included).

**Choice.** Shell out to the `sops` binary, behind `internal/sops.Encryptor` (production: `sops.Binary{}`; tests can inject a fake), the same pattern `internal/git` uses for `git` and `internal/github` for `gh`. `go.mod`/`go.sum` are unchanged by this ticket. The developer-facing cost is one more binary dependency, documented in the README next to git and gh; `sops.Available()` gives a clear "not installed" error instead of a raw exec failure, matching `internal/git`'s and `internal/github`'s handling of a missing binary.

**Consequence for CI.** `.github/workflows/ci.yaml`'s `go` job is deliberately tool-free (its own comment: "The chart tests skip themselves when helm or kubeconform is missing, so the Go job above stays tool-free"); this ticket's files may not touch workflows, so the `sops` binary is not installed there either. The tests that need it (`internal/sops`'s and `internal/cli`'s secret set tests) skip themselves when `sops` is not on `PATH`, exactly like the chart tests skip on a missing `helm`/`kubeconform`. They run for real wherever `sops` is present (it is on the machine this ticket was implemented on: `sops --decrypt` round-trips the fixture key). Wiring a CI job that installs `sops` and forces these tests to run (an `IIDP_REQUIRE_SOPS_TOOLS=1` env var, mirroring `IIDP_REQUIRE_CHART_TOOLS`) is a natural follow-up for whoever next touches the workflows.

## The command line the sops binary is invoked with

The plaintext Kubernetes Secret document is built in memory (`render.SecretDocument`) and piped to `sops` over stdin; the encrypted document comes back over stdout. It never touches disk unencrypted, which is stronger than "never reaches disk unencrypted inside the clone": it never reaches disk at all.

```sh
sops --encrypt --input-type yaml --output-type yaml \
  --age <agePublicKey> --encrypted-regex '^(data|stringData)$' \
  --filename-override <path/of/the/file.enc.yaml> /dev/stdin
```

`--age` and `--encrypted-regex` are passed explicitly rather than relying on a `.sops.yaml` creation rule, because `applications/<app>/<env>/sops/` has no such file (only `bootstrap/sops/` does, scoped to itself by `path_regex`) and explicit flags always win over a config file's rules regardless. `--filename-override` is required by `sops` whenever the plaintext arrives over stdin; it is passed the file's real repository-relative path so a future error message or config match is meaningful, even though nothing reads or writes that path directly. `--input-type`/`--output-type yaml` make the invocation self-contained: it does not depend on `--filename-override` ending in `.yaml`, or on the working directory.

Manually verified against a real `sops` binary and the fixture age key pair (`test/e2e/fixtures/age-keys.txt`, public key `age1kpq9t46wreydm6dp2e9a6txzm88ymqj9ph38jvjlsjgff3k5vfqqqhee6v`, also in `test/e2e/fixtures/platform-repo/platform.yaml`): the output has the exact shape `bootstrap/sops/*.enc.yaml` in the fixture already has (only `stringData` encrypted, `sops.age` with one recipient, `encrypted_regex`, `mac`, `version`), and `sops --decrypt` round-trips it.

## Layout: one Secret, one file, per key

Chosen, as the ticket recommends: `applications/<app>/<env>/sops/<key-slug>.enc.yaml` holds a Secret named `<fullname>-<key-slug>` (`shop-api-key` for prod, `shop-staging-api-key` for staging, `fullname` being the chart's own `_helpers.tpl` `application.fullname`: the bare Application name for prod, `<name>-staging` for staging). One file per key, not one file per `secret set` invocation or one file for the whole Environment, because the CLI holds no private key and cannot merge into an existing encrypted document (`docs/platform-repository.md`, "Secrets"); updating one key means only that key's file changes, which the tests assert directly (`TestSecretSetUpdatingAnExistingKeyOnlyTouchesItsFile`).

`kustomization.yaml` (`generators: [ksops.yaml]`) and `ksops.yaml` (the KSOPS generator, `metadata.name: <fullname>-secrets`, `files:` every `*.enc.yaml` in the directory, sorted) are rewritten on every `secret set`, from a directory listing taken after the new or updated file is written, so a second key added later is picked up without the CLI having to track state of its own. Rewriting them is idempotent: `kustomization.yaml`'s content never changes and does not appear in a commit past the first `secret set` for an Environment; `ksops.yaml` only changes when the set of files changes.

The Secret carries `kustomize.config.k8s.io/needs-hash: "false"` (so its name stays what `values.yaml` and `ksops.yaml` reference) and `argocd.argoproj.io/sync-wave: "-2"`, one wave before the migration Job's `"-1"` (notes for #7), so the Job's `envFrom` secrets exist before it runs. No `namespace`: the ArgoCD Application's `spec.destination.namespace` applies, the same reasoning `docs/platform-repository.md` already gives for the wildcard TLS secret. Labels are the identity pair every chart object carries for Grafana Alloy attribution, `iidp.itema.no/application` and `iidp.itema.no/environment`, plus `app.kubernetes.io/name` (the Application name); not `app.kubernetes.io/instance`, since a Secret is not selected by anything and that label is reserved for the chart's own selector-bearing objects.

## `values.yaml` and `application.yaml` are edited, not rewritten

Both files already exist (written by `iidp app create`) and carry a header comment and, for `values.yaml`, values the CLI does not otherwise touch (`env`, later Capabilities). Rewriting them from scratch would either lose that content or require `secret set` to know the whole values shape, which is `app create`'s and `app add-capability`'s job, not this one's.

**Choice.** `gopkg.in/yaml.v3`'s `yaml.Node` tree: decode the file into a document node, find or create the `secrets:` sequence (`render.AddSecretName`) or the third `spec.sources` entry (`render.AddKustomizeSource`), append if the value is not already there, and re-encode with `SetIndent(4)` (the same indent `yaml.Marshal`, and so every other file this CLI writes, already uses). This round-trips comments: `internal/render/edit_test.go` asserts the two-line header comment on both fixtures survives the edit. Both functions report whether they changed anything, so `SetSecrets` only rewrites (and only commits) a file that actually changed — the reason setting a second key touches `ksops.yaml` and `values.yaml` but not `kustomization.yaml` or `application.yaml` (`TestSecretSetAddingASecondKeyOnlyTouchesKsopsAndValues`), and updating an existing key's value touches only that key's `.enc.yaml` (`TestSecretSetUpdatingAnExistingKeyOnlyTouchesItsFile`): git only shows files with a real diff in `git show --name-only`, and re-encrypting the same key always produces a different ciphertext (a fresh nonce), so there is always something to commit even when nothing else changed.

The third source added to `application.yaml` is matched by `path` alone (only one source in an Environment's Application ever sets one), and is otherwise exactly the multi-source kustomize form `docs/platform-repository.md` and #10's notes already use for the chart source's sibling: `repoURL` the Platform repository (`internal/platform.RepositoryURL`, the same constant `render.ArgoCDApplication` already writes into source 1), `targetRevision: main`, `path: applications/<app>/<env>/sops`. Confirmed against the current Argo CD documentation ("Multiple Sources for an Application"): a multi-source Application may combine a Helm source, a `ref`-only values source and a plain git source with a `path`, which ArgoCD detects as kustomize (it finds `kustomization.yaml`) and renders through the repo server's `ksops`/`--enable-alpha-plugins --enable-exec` setup from #4's bootstrap notes, exactly like `bootstrap/platform-secrets.yaml` already does for `bootstrap/sops/`.

## Environment and KEY validation

`platformrepo.ValidateEnvironmentName` (prod or staging: the only two Environment directories the layout has) and `platformrepo.ValidateSecretKey` (a POSIX environment variable name: letters, digits and underscores, not starting with a digit) live next to `ValidateName` in `internal/platformrepo`, not in `internal/cli`, because — like the Application name — they constrain what the Writer may put into the Platform repository's layout and file names, and a later command (`app add-capability`, say) might need the same Environment check. `SecretSlug` (the KEY lowercased, `_` to `-`) is exported alongside them for the same reason.

All of it, plus reading `--from-file` and `--stdin` values and rejecting a KEY given more than once in one invocation, runs before the GitHub token is even requested (`collectSecrets` in `internal/cli/secret.go`), matching `app create`'s `createOptions.application()`: a bad flag is refused with no network call and no clone. What still requires a clone — the Environment directory existing and `platform.yaml` setting `agePublicKey` — is checked immediately after `LoadConfig`, before any file is written, the same as `app create`'s "Application already exists" check.

## Commit message lists keys, not values

`iidp secret set <app> <env> KEY1 KEY2 ...`, the KEYs in the order given on the command line (flag values interleaved with `--from-file`/`--stdin` keys in the order they were parsed: positional pairs, then `--from-file`, then `--stdin`). Never a value; `TestSecretSetEncryptsAndCommitsAKeyFromAFlag` asserts the plaintext appears nowhere in the commit message or the committed tree (`git grep` over the whole tree at `HEAD`).

## `--stdin` accepts at most one KEY per invocation

Stdin is one stream; a second `--stdin KEY` would either read nothing (already consumed) or block. Refused up front, before the first is read, alongside the other flag-shape checks.

## Trimming file and stdin values

`--from-file` and `--stdin` both trim exactly one trailing line ending (`\n`, or `\r\n`) from the value, the way a value typed into an editor or produced by `echo` commonly carries one that is not part of the intended secret. A value on the command line (`KEY=value`) is used exactly as given: the shell has already decided its content, and there is no trailing newline to strip.

## Testing

The CLI seam (`cli.Run`/`cli.RunWith` against a bare git repository, as `docs/design.md`'s testing seams and #10's notes already establish) drives every test; `internal/render`'s new functions additionally get `Example` tests (`SecretDocument`, `SopsKustomization`, `KsopsGenerator`) and table tests for the node-editing functions, the same split #10 used for the two existing documents. `internal/sops` gets its own unit tests for the `Encryptor` shelling out correctly and failing clearly when `sops` is missing (simulated with `PATH=""`, not by requiring the developer's own machine to lack it).

Round-trip decryption uses the real `sops --decrypt` binary with `SOPS_AGE_KEY_FILE` pointed at the fixture private key (`test/e2e/fixtures/age-keys.txt`), not the `sops/v3/decrypt` Go package: having decided against the library for production, adding it back only for tests would reintroduce the same dependency weight (it is a real import, not conditionally compiled away) for no offsetting benefit, and shelling out to `sops` for both encryption and decryption keeps the whole feature's tool dependency to one binary. Every test that shells out to `sops` skips itself when the binary is not on PATH (see "Consequence for CI" above).

`TestSecretSetProducesAKustomizationKsopsCanBuild` additionally runs `kustomize build --enable-alpha-plugins --enable-exec` on the produced `sops/` directory and asserts the decrypted Secret comes out, when `kustomize` and `ksops` are both on `PATH` (they were not on the machine this ticket was implemented on, so this test is unexercised here; it is written and will run wherever both are installed).
