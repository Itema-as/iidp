# #76 Itema login on custom domains inside cloudflareZone

Itema login used to be refused together with any custom domain, because oauth2-proxy set its cookie for `.<baseDomain>` (`.app.itma.no`) and only redirected back to hosts under it. It now works with any custom domain inside `platform.yaml`'s `cloudflareZone` (`itma.no`), for example `x.itma.no` or `butikk.app.itma.no`. A domain outside the zone is still refused, and the refusal names it.

Sources were checked on 2026-09-26:

- oauth2-proxy's configuration page, [`configuration/overview`](https://oauth2-proxy.github.io/oauth2-proxy/configuration/overview) (7.15.x docs), for `--cookie-domain`, `--whitelist-domain`, `--cookie-name`, `--cookie-expire` and `--cookie-refresh`.
- oauth2-proxy's source at tag [`v7.15.3`](https://github.com/oauth2-proxy/oauth2-proxy/tree/v7.15.3), the appVersion `bootstrap/versions.yaml` pins. The files are linked below.
- cert-manager's source at [`v1.21.2`](https://github.com/cert-manager/cert-manager/tree/v1.21.2) and Traefik's at [`v3.7.8`](https://github.com/traefik/traefik/tree/v3.7.8), both the pinned versions.

## oauth2-proxy: cookie domain and redirect allowlist

The docs describe `--cookie-domain` as "Optional cookie domains to force cookies to (e.g. `.yourcompany.com`). The longest domain matching the request's host will be used (or the shortest cookie domain if there is no match)". They describe `--whitelist-domain` as "allowed domains for redirection after authentication. Prefix domain with a `.` or a `*.` to allow subdomains". The source matches both descriptions:

- [`pkg/cookies/cookies.go` `GetCookieDomain`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/cookies/cookies.go#L61) picks the first configured domain the request host (`X-Forwarded-Host` in reverse-proxy mode) ends with. `validateCookie` sorts the list longest first. With one entry, `.itma.no`, every host under it gets `Domain=.itma.no`, and so do `auth.app.itma.no` on the callback and `x.itma.no` on the sign-in start (the CSRF cookie). A host that matches nothing still gets the only domain configured, with a logged warning ([`MakeCookieFromOptions`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/cookies/cookies.go#L27)). The zone apex `itma.no` is such a host: `"itma.no"` does not end with `".itma.no"`. It still works, because a browser accepts `Domain=.itma.no` from `itma.no` (RFC 6265 drops the leading dot).
- [`pkg/util/util.go` `isHostnameAllowed`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/util/util.go#L169) allows the domain itself, without its dot, and any host ending in `.itma.no`. It also requires the redirect's port to be empty when the entry has none, which is always true for `https://<host>/...`. So the return URL oauth2-proxy builds from `X-Forwarded-*` for `x.itma.no` passes, and `notitma.no` does not.

The callback stays `https://auth.<baseDomain>/oauth2/callback`, so the Entra app registration does not change. The bootstrap now refuses a `baseDomain` outside `cloudflareZone`. The callback host sets the cookie, and a browser drops a cookie whose `Domain` does not cover the host that set it.

## The duplicate cookie at rollout: renamed

**Question.** After the change, a browser signed in before it holds `_oauth2_proxy` for `.app.itma.no`, and gets a new `_oauth2_proxy` for `.itma.no` at its next sign-in. Both are sent to every `*.app.itma.no` host. Does oauth2-proxy cope without a loop?

**Finding: not reliably.** Rename.

- [`pkg/sessions/cookie/session_store.go` `loadCookie`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/sessions/cookie/session_store.go#L228) takes the first cookie of the name (`req.Cookie`). For a session too big for one cookie it takes the first `_0`, the first `_1`, and so on, and joins them. Browsers order same-path cookies by creation time, oldest first (RFC 6265 5.4), so the old cookie is read and the new one ignored.
- While the old cookie is valid, that is harmless. It is signed with the same secret, and with `cookie-refresh` at its default 0 nothing else expires it before the browser does ([`needsRefresh`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/middleware/stored_session.go#L244)). Refresh is off, so `validateSession`'s expiry check never runs.
- It loops once a split session's parts stop lining up. Entra sessions carry an ID, access and refresh token, which is likely more than one 4 KB cookie. If the new session needs more parts than the old one, the joined value is old `_0` + old `_1` + new `_2`, and its signature fails. [`loadSession`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/middleware/stored_session.go#L107) then calls [`Clear`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/sessions/cookie/session_store.go#L73). That clears cookies only for the configured domain (`.itma.no`), never the old `.app.itma.no` ones. So every sign-in ends where it started. The same `Clear` means `/oauth2/sign_out` would leave the old cookie, and the user signed in on `*.app.itma.no`, for up to 7 days.
- The CSRF cookie, `<cookie-name>_csrf` ([`csrfCookieName`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/cookies/csrf.go#L290)), has the same problem for the 15 minutes it lives. [`LoadCSRFCookie`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/cookies/csrf.go#L77) takes the first one that decodes, which may belong to another sign-in.

Not confirmed on the live Platform: how many parts a real Entra session has. The loop needs the new session to be split into more parts than the old one. It is possible, not certain, and the sign-out problem holds either way.

**Choice.** `cookie-name: __Secure-itema_login`. oauth2-proxy never reads the old cookie again, and it expires on its own. Everyone signs in once more after the upgrade, which is silent with an Entra session. The `__Secure-` prefix is what the docs recommend with `cookie-secure`. It needs `Secure` and allows a `Domain`, which `__Host-` would not. The name passes [`validateCookieName`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/validation/cookie.go). The cookie secret is unchanged. An Application never sees the cookie, only the `X-Auth-Request-*` headers, so nothing downstream depends on the name.

Rejected: keeping the name and waiting it out. The loop above would last up to 7 days per browser, with no workaround but clearing cookies by hand.

## The accepted trade-off

The browser sends the cookie to every host under `itma.no`, including hosts the Platform does not run: the website, and SaaS services behind a CNAME. Whoever controls one of them, or can read its request logs, can replay a signed-in user's session against every login-protected Application until it expires. #76 accepted this. It is recorded in `bootstrap/README.md` ("Itema login") and `docs/platform-repository.md`, next to `--login`, and linked from `README.md` and `chart/application/README.md`.

## Chart: `platform.loginCookieDomain`

The chart cannot see `platform.yaml`, so the cookie domain is a platform value next to `baseDomain` and `httpIssuer`: `platform.loginCookieDomain`, written without the leading dot. Empty means `baseDomain`. That is the bootstrap's fallback, and it keeps every values file written before #76 rendering exactly as before.

- `application.validate` no longer refuses `login.enabled` with `domains`. `application.login.checkDomains` refuses when a custom domain is not the cookie domain or under it, and names every such domain. It also refuses when the Platform address itself is outside the cookie domain, since a wrong hand-set value would lock the Platform address out too.
- `application.login.annotation` is the one place the middleware annotation is spelled. `ingress.yaml` and `ingress-http01.yaml` both include it. Hosts that are not a single label under `baseDomain`, such as `x.itma.no`, `test.shop.app.itma.no` or the apex, go on the HTTP-01 Ingress.

**Why not derive it in the chart.** The chart could guess the zone by stripping `baseDomain`'s first label. `platform.yaml` does not promise that shape, and the bootstrap and the CLI already read `cloudflareZone` itself. The CLI writes the value, following the same pattern as `backupsBucket` and `objectStorageEndpoint`.

## cert-manager's HTTP-01 challenge does not go through ForwardAuth

The challenge for a protected host on the HTTP-01 Ingress must reach cert-manager's solver, not oauth2-proxy.

- cert-manager creates its own Ingress for each challenge ([`pkg/issuer/acme/http/ingress.go`](https://github.com/cert-manager/cert-manager/blob/v1.21.2/pkg/issuer/acme/http/ingress.go#L147)). It has the issuer's `ingressClassName` (`traefik`), one rule for the host with the path `/.well-known/acme-challenge/<token>`, and no Traefik annotation. The path is `Exact`: the `ACMEHTTP01IngressPathTypeExact` feature gate is on by default in v1.21 ([`internal/controller/feature/features.go`](https://github.com/cert-manager/cert-manager/blob/v1.21.2/internal/controller/feature/features.go)). cert-manager adds the path to an existing Ingress only when the solver names one (`http01.ingress.name`, [line 91](https://github.com/cert-manager/cert-manager/blob/v1.21.2/pkg/issuer/acme/http/ingress.go#L91)). The bootstrap's `letsencrypt-http01` names none, and must not start to: the path would then carry the Application Ingress's middleware.
- Traefik's Kubernetes Ingress provider builds one router per Ingress path, and gives it only that Ingress's own annotations, the middlewares included ([`pkg/provider/kubernetes/ingress/kubernetes.go`](https://github.com/traefik/traefik/blob/v3.7.8/pkg/provider/kubernetes/ingress/kubernetes.go#L750)). `Exact` becomes `Path(...)`. With no explicit priority, a router's priority is the length of its rule ([`pkg/server/router/router.go`](https://github.com/traefik/traefik/blob/v3.7.8/pkg/server/router/router.go), [`pkg/muxer/http/mux.go` `GetRulePriority`](https://github.com/traefik/traefik/blob/v3.7.8/pkg/muxer/http/mux.go)). ``Host(`x.itma.no`) && Path(`/.well-known/acme-challenge/<token>`)`` is longer than ``Host(`x.itma.no`) && PathPrefix(`/`)``, so the solver's router wins for the challenge path, with no middleware.
- Let's Encrypt asks over plain HTTP. The bootstrap's `web` entrypoint redirects everything to HTTPS before any router runs. The validator follows the redirect (see `08-chart-static-domains-secrets.md`) and meets the same routers on `websecure`. The solver's Ingress has no `router.tls` annotation, but it does not need one there. The k3s Traefik chart (`40.1.4+up40.1.0`, rendered with the harness's values) runs `--entryPoints.websecure.http.tls=true`, which makes every router on that entrypoint a TLS router, and redirects `web` to `:443`.

**Tested.** kind cannot run a real challenge, because Let's Encrypt never issues for `.test`. `TestBootstrap` (`Cluster.CheckACMEChallengeBypassesLogin`) instead creates an Ingress of the solver's exact shape for `shop-staging.example.test`, backed by the fixture's nginx, next to the protected Environment's own HTTP-01 Ingress. It requires `https://shop-staging.example.test/.well-known/acme-challenge/e2e-token` to be answered by nginx (404 with nginx's `Server` header), and a redirect to sign-in fails the check. It also requires the `http://` URL to redirect to that `https://` URL. `harness_test.go` checks the check itself against local servers.

## CLI

- **Where the refusal is.** `createOptions.plan` and `runAppAddCapability` validate flags before anything is cloned, and they cannot know `cloudflareZone`. Their flag-only refusal is gone, and `platformrepo.CheckLoginDomains` does the check once `platform.yaml` is read:
  - For `--path create` and `--path adopt`, it runs on the clone `CheckAvailable` already makes, before the Application repository is created or the pull request opened.
  - In the Writer, `attemptCreate` covers the bare path and the wizard's preview, and `attemptAddCapabilities` covers `add-capability`. Both check before anything is written.

  The messages name every domain outside the zone and the zone itself.
- **`add-capability`** refuses `--login` when prod's existing domains or the ones given with it are outside the zone. It also refuses `--domain` outside the zone for an Application that already has login, which was never checked before: the chart refused it at sync time. `--login` writes `platform.loginCookieDomain` into every Environment. An in-zone `--domain` on a protected Application rewrites it in prod, because an Environment written before #76 has no value and the chart would check the new domain against `baseDomain`.
- **What is written.** `render.Environment.LoginCookieDomain` becomes `platform.loginCookieDomain`, only with `--login`, the way the Postgres fields appear only with `--postgres`. `render.EnableLogin` takes the cookie domain.
- **The wizard** asks question 7 when no custom domain was given, or when every one is inside the zone. To know the zone it reads `platform.yaml` (`Writer.ReadConfig`), once and only when domains were given. When it skips the question it prints which domain is outside the cookie domain. Asking and then refusing in the preview was the alternative, and it would ask a question whose "yes" cannot work.

## Rollout

- The chart change needs a chart release. An Environment pinned to an older chart still refuses `login.enabled` with any domain. `add-capability --domain` on such an Environment writes values its chart refuses, and ArgoCD shows the sync error. Bump its chart first. New Environments get `platform.yaml`'s `chartVersion`, so bump that after the release too.
- The bootstrap change reaches the Platform when `platform-components.yaml`'s pin moves. Everyone signs in once more (the renamed cookie). The old `_oauth2_proxy` cookies are ignored and expire within 7 days.
- ADR-0004 still says the cookie is for `.app.itma.no`. It records the decision as taken then and is left as it is.

## Tests

- `bootstrap`: `TestOauth2ProxyCookieDomain` checks `cookie-domain` and `whitelist-domain` with a zone, with the zone equal to `baseDomain`, and without a zone, plus `cookie-name` and the refusal of a `baseDomain` outside the zone. The existing oauth2-proxy test now expects the fixture zone.
- `chart/application`: `login-custom-domains.yaml` has the middleware on both Ingresses and the issuer kept. There is a base-domain fallback case, `refuse-login-domain-outside-cookie-domain.yaml` (naming `shop.example.com` and `notitma.no`, not `x.itma.no`), `refuse-login-platform-address-outside-cookie-domain.yaml`, no middleware on the HTTP-01 Ingress without login, and kubeconform on the new fixture. `refuse-login-with-domain.yaml` is gone.
- `internal/render`: `EnableLogin` writes and rewrites the cookie domain.
- `internal/cli`:
  - `app create` covers in-zone domains, a refusal naming only the out-of-zone domain, the Create path refusing before the repository exists, and the `baseDomain` fallback without a zone.
  - `add-capability` covers `--login` with in-zone and out-of-zone domains, given together or already present, and `--domain` in and out of the zone on a protected Application, including one whose values predate the field.
  - The wizard offers login for in-zone domains and skips it, saying why, for an out-of-zone one.
- `test/e2e`: `shop-staging` lists `shop-staging.example.test`, inside the fixture zone and on the HTTP-01 Ingress. `TestBootstrap` requires its sign-in redirect: Entra's authorize endpoint, the callback on `auth.app.example.test`, the state ending in the custom domain's URL, and a `__Secure-itema_login_csrf` cookie for `example.test`. It also runs the challenge check above. The CLI writes domains for prod only. The chart takes them on any Environment, so the fixture puts the domain on its one protected Environment rather than adding another.

Not covered by an automated test: a completed sign-in against the real Entra ID on a custom domain, and a real HTTP-01 issuance for a protected host. Both are checked by hand on the Platform once a release carrying this is there.
