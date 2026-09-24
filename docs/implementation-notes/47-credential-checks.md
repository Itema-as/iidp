# #47 Checking Grafana Cloud and Entra credentials before encrypting them (items 6, 7, 8 and 11)

The bootstrap wizard encrypts the Grafana Cloud and Entra values with the Platform's age key. The private key exists only in the cluster, so a wrong value can't be edited afterwards: the fix is a full wizard re-run. Both mistakes below happened on the real Platform. The wizard now proves each value before it encrypts anything, like the Cloudflare stage already did. Real endpoints were probed with deliberately invalid credentials on 2026-09-24.

## Item 6: where the Grafana values live

`https://grafana.com/orgs` has no page behind it without an org slug. The stage now prints `https://grafana.com/auth/sign-in`. After sign-in the Cloud Portal opens on the admin's organisation. The stage then says where each value is:

- The stack's **Prometheus** card, **Details**: the remote write endpoint, and the username (instance id).
- The stack's **Loki** card, **Details**: the URL, which is a bare host, and the user (instance id).
- **Security → Access Policies**: create a policy with the stack as its realm and the scopes `metrics:write` and `logs:write`, then **Add token** on it.

The URL is printed with `print_url`, like every other stage's link (#47 item 5).

## Item 7: bare hosts become push URLs

`grafana_push_url KIND URL` runs on both answers before anything else sees them:

- It trims whitespace, adds `https://` when no scheme is given, and strips trailing slashes.
- For a bare host it appends `/loki/api/v1/push` (Loki) or `/api/prom/push` (Prometheus).
- For Prometheus it also completes `/api/prom` to `/api/prom/push`. `/api/prom` is the query base the portal shows next to the push URL, and it's the same trap as a bare host.
- Any other path is left alone. A typo in a full URL is caught by the probe below, not by guessing.

When the URL changes, the wizard logs the URL it will store, so the admin sees what was stored.

## Item 8: one empty, authenticated push to each URL

**Choice.** The wizard POSTs a push that carries no data to each push URL, with HTTP basic auth (instance id, token):

- **Loki:** `{"streams":[]}` with `Content-Type: application/json`.
- **Prometheus:** a remote write with no series. An empty protobuf `WriteRequest` is zero bytes, and Snappy-compressed it becomes the single byte `0x00` (the varint for length 0). It goes out with `Content-Type: application/x-protobuf`, `Content-Encoding: snappy` and `X-Prometheus-Remote-Write-Version: 0.1.0`. A shell string can't hold a NUL byte, so `http_post` now sends its body with `--data-binary` and takes `@FILE` for this one case. It also takes extra headers.

**What the real gateways answer.** `logs-prod-025.grafana.net` and `prometheus-prod-24-prod-eu-west-2.grafana.net` behave the same way. The path is checked before the credentials:

| Request | Answer |
|---|---|
| bare host (`POST /`) | 405 (what Alloy got) |
| wrong path | 404 `404 page not found` |
| right path, wrong token | 401 `authentication error: invalid token` |
| right path, no credentials | 401 `authentication error: no credentials provided` |

So 404/405 means the URL is wrong, whatever the credentials, and 401/403 means the URL is right and the user or token is wrong. The wizard dies with a message naming what to fix: the Details page for a URL, the instance id and the policy's scope and realm for credentials. It also says nothing has been encrypted.

**Why not a read-only probe.** `/api/prom/api/v1/query` on the same host is read-only, but it needs `metrics:read`. The token the Platform uses has only `metrics:write` and `logs:write`, so a query would be refused even with correct credentials. The push endpoints are the ones Alloy uses, with the scopes Alloy has, so they prove exactly what matters. An empty push writes no samples and no log lines.

**What I could not check.** I had no valid credentials, so the success status is taken from the APIs rather than observed. Loki answers a successful push with 204. Mimir, behind Grafana Cloud's Prometheus endpoint, returns 200 for a request with no series. The wizard accepts any 2xx. Any status that is neither 2xx nor a known refusal (for example a 400 if Mimir rejected the empty body, or a 5xx) gets a warning with the status and body. The admin is then asked whether to encrypt anyway. This way an unexpected answer can't block a correct setup, and the two known mistakes still stop the run. If the first real run shows a 400 for the empty remote write, it means the credentials passed the gateway. The fallback covers it, but the probe should then be changed.

A host that doesn't resolve fails in `_http` itself ("network call failed"), as it does for the Cloudflare and Hetzner checks.

## Item 11: the Entra client secret

**The mix-up.** The Azure portal lists each client secret's **Value** and its **Secret ID** side by side. The ID is a GUID. The Value is a roughly 40-character string that is never GUID-shaped. On the manual path (no logged-in `az`), `entra_register_app` now says which column to copy. It refuses a GUID-shaped answer with "that's the Secret ID; paste the Value". It also says that the Value is shown only right after the secret is created, so a new secret may be needed.

**The proof.** The wizard then requests a token from `https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token`, with `grant_type=client_credentials` and `scope=https://graph.microsoft.com/.default`. This checks the tenant, the client id and the secret together. A client-credentials token for Graph's `.default` scope needs no application permission granted. Entra issues it with an empty `roles` claim, so it needs nothing beyond what the manual steps create. Any status other than 200 with an `access_token` is a refusal. The wizard dies with Entra's own `AADSTS…` message, without the trace and correlation ids, which mean nothing to the admin. Probed with invalid values, Entra answers with the reason in the message: `AADSTS900023` for a malformed tenant, `AADSTS700026`/`AADSTS700016` for a client id, `AADSTS7000215` for a wrong secret. Its `AADSTS7000215` text itself says "the client secret value, not the client secret ID". The form values are URL-encoded with `jq`'s `@uri`.

The message also says that a secret created a moment ago can take a minute to work. Entra doesn't guarantee a new credential is usable immediately, and a refusal only costs a re-run of the answers, since nothing has been encrypted yet.

**Why only the manual path.** The `az` path creates the secret itself, so it can't paste the ID by mistake. It also creates the app with `az ad app create`, which doesn't create a service principal. A client-credentials request needs one in the tenant (`AADSTS7000229` otherwise), and it would also hit the new-secret delay straight away. A portal registration, which is what the manual steps describe, creates the service principal automatically.

## Fake mode

As with the Cloudflare checks, `_fake_http` answers both new endpoints under `IIDP_WIZARD_FAKE=1`, so `test/wizard/run.sh` stays offline. `_http` now passes the auth header and body to it.

- **Grafana** (`https://*.grafana.net`) copies the real gateway's order. A bare host answers 405 and any other path 404. A push path answers 204 when the basic-auth user is numeric and the token equals `IIDP_WIZARD_FAKE_GRAFANA_TOKEN` (default `fake-token`), and 401 otherwise.
- **Entra** answers 200 with an access token when `client_secret` equals `IIDP_WIZARD_FAKE_ENTRA_SECRET` (default `fake-secret-value`). Otherwise it answers 401 with the real `AADSTS7000215` body.

The tests cover the following:

- **URL completion:** a bare host with and without a trailing slash, a full URL unchanged, a trailing slash after a full URL, Prometheus's `/api/prom`, and a missing scheme.
- **`stage_grafana`:** the sign-in link and where-to-find text, success with bare hosts (stored as full push URLs), full URLs kept, a non-push URL refused (404), a wrong token refused (401) and a wrong user refused (401).
- **`entra_register_app` on the manual path:** success, a Secret ID refused, and a secret Entra rejects refused.
