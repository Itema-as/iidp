# #148 Itema login on custom domains outside the login cookie domain

Itema login used to be refused on a custom domain outside `platform.loginCookieDomain` (`cloudflareZone`, or `baseDomain` without one): the shared oauth2-proxy's cookie is set for `.itma.no` and never reaches `shop.example.com` or `iprofil.itema.no`. Such a domain, an out-of-zone host, can now be behind Itema login. It signs in through a second oauth2-proxy, the host-only one, whose callback and cookies stay on the host. In-zone hosts and every Platform address are unchanged.

Sources were checked on 2026-10-06, at the versions `bootstrap/versions.yaml` and the harness pin:

- oauth2-proxy's source at tag [`v7.15.3`](https://github.com/oauth2-proxy/oauth2-proxy/tree/v7.15.3), and its Helm chart at [`oauth2-proxy-10.7.0`](https://github.com/oauth2-proxy/manifests/tree/oauth2-proxy-10.7.0/helm/oauth2-proxy), rendered with `helm template`.
- Traefik's source at [`v3.7.8`](https://github.com/traefik/traefik/tree/v3.7.8).
- cert-manager's source at [`v1.21.2`](https://github.com/cert-manager/cert-manager/tree/v1.21.2).
- Kubernetes' Ingress validation at [`v1.36.4`](https://github.com/kubernetes/kubernetes/blob/v1.36.4/pkg/apis/networking/validation/validation.go), the k3s release's version.

## The host-only oauth2-proxy

The bootstrap's `oauth2-proxy` Application gets a third source: the same pinned chart again, `releaseName: oauth2-proxy-host`, `fullnameOverride: oauth2-proxy-host`, no Ingress. It runs on every Platform, whether or not any Application uses it, so turning login on for an out-of-zone host never waits for a bootstrap change. It costs one more Pod of the same size (10m CPU, 32Mi).

- **Shared with the shared proxy:** the Entra registration, client secret and cookie secret (the same `oauth2-proxy-entra` Secret, so no new SOPS key), the provider and its claims, `injectResponseHeaders`, `skip-provider-button`, `footer`, `session-cookie-minimal` and `cookie-secure`. They come from one helper, `bootstrap/templates/_oauth2-proxy.tpl`. `TestHostOnlyOauth2ProxyIsTheSharedOneWithHostOnlyCookies` requires the two `valuesObject`s to be equal apart from the four arguments below, `fullnameOverride` and `ingress`.
- **Different:** no `redirect-url`, no `cookie-domain`, no `whitelist-domain`, and `cookie-name: __Host-itema_login`.
- **The shared proxy's rendered config is unchanged.** Its source, and every other Application, render identically before and after, compared as parsed YAML with the e2e fixture and with the default values. `TestSharedOauth2ProxyIsPinned` keeps it so: it compares the shared source's `helm` block with `bootstrap/testdata/oauth2-proxy-shared-source.yaml`, rendered from the commit before this change.
- **Two releases of one chart do not collide.** Every object is named after `fullnameOverride`, except the alpha-config Secret `<fullname>-alpha`. Nothing is cluster-scoped. The pod selector is `app.kubernetes.io/name: oauth2-proxy` plus `app.kubernetes.io/instance: <Release.Name>` ([`_helpers.tpl` L54](https://github.com/oauth2-proxy/manifests/blob/oauth2-proxy-10.7.0/helm/oauth2-proxy/templates/_helpers.tpl#L54)). So it is `releaseName`, not `fullnameOverride`, that keeps one Service from selecting the other's Pods. ArgoCD tracks resources by annotation, so it leaves that label alone.
- **Its Middleware** is `oauth2-proxy/itema-login-host-auth` in `bootstrap/components/oauth2-proxy-login/middleware-host.yaml`: `itema-login-auth` with the address `http://oauth2-proxy-host.oauth2-proxy.svc.cluster.local/`. `TestLoginMiddlewares` requires everything else to be equal. It is a file of its own because several tests read `middleware.yaml` as a single document.

Rejected: a separate ArgoCD Application. The brief allows either, and one Application keeps both proxies, the Middlewares and the Secret they share in one sync.

### The callback is built from the requested host

- With `redirect-url` empty, [`getOAuthRedirectURI`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/oauthproxy.go#L1113) fills in the host from `GetRequestHost`, which reads `X-Forwarded-Host` in reverse-proxy mode. It sets the scheme to `https`, forced by `cookie-secure`. Both the sign-in start ([`doOAuthStart`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/oauthproxy.go#L867)) and the code redemption ([`redeemCode`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/oauthproxy.go#L985)) use it, so both send `https://<host>/oauth2/callback`.
- Traefik's entrypoint drops `X-Forwarded-*` from clients it does not trust and sets `X-Forwarded-Host` to the request's Host ([`forwarded_header.go`](https://github.com/traefik/traefik/blob/v3.7.8/pkg/middlewares/forwardedheaders/forwarded_header.go#L171)). ForwardAuth with `trustForwardHeader: true` passes it on ([`forward.go` L418](https://github.com/traefik/traefik/blob/v3.7.8/pkg/middlewares/auth/forward.go#L418)).
- **`--trusted-proxy-ip` is left at its default.** Empty means `0.0.0.0/0, ::/0` with `reverse-proxy` ([`buildTrustedProxyNetSet`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/oauthproxy.go#L401)), so oauth2-proxy reads the headers from Traefik. Narrowing it to the wrong range would make oauth2-proxy ignore `X-Forwarded-Host`, and the callback would carry the Service's name. The shared proxy has the same default, and only Traefik reaches the Service, so it gains nothing here.

### Cookie name `__Host-itema_login`

- [`validateCookieName`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/validation/cookie.go#L39) only checks that Go can write the cookie and that the name is under 256 characters. It has no rule about the `__Host-` prefix.
- With no `cookie-domain`, [`MakeCookieFromOptions`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/cookies/cookies.go#L27) leaves `Domain` empty, and Go writes no `Domain` attribute. Path is `/`, and the cookies are Secure and HttpOnly. That is everything a browser requires of a `__Host-` cookie, for the session cookie and for the CSRF cookie `__Host-itema_login_csrf` ([`csrfCookieName`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/cookies/csrf.go#L290)). A session split into parts gives `__Host-itema_login_0` and so on, which keep the prefix.
- The prefix makes a browser refuse the cookie if a later change ever gives it a `Domain`, so the cookie can never widen silently.
- The value's signature covers the cookie's name ([`encryption/utils.go` L45](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/encryption/utils.go#L45)). So a `__Secure-itema_login` value, signed with the same secret, is not a valid `__Host-itema_login`, and the other way round.

### `whitelist-domain` stays empty

**Question.** Without a whitelist, oauth2-proxy refuses the return URL it builds from the `X-Forwarded-*` headers. Set one, or accept the log line?

**Finding.**
- [`GetRedirect`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/app/redirect/director.go#L57) builds `https://shop.example.com/account/orders?page=2` from the headers ([`getXForwardedHeadersRedirect`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/app/redirect/getters.go#L35)). [`IsValidRedirect`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/app/redirect/validator.go#L41) refuses it: with an empty list no absolute URL is allowed.
- oauth2-proxy logs `Invalid redirect generated from X-Forwarded-* headers: …` ([L85](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/app/redirect/director.go#L85)). It falls back to `X-Forwarded-Uri`, the path and query alone ([`getURIRedirect`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/app/redirect/getters.go#L60)).
- The state is `<csrf>:/account/orders?page=2`. The callback checks it again ([L955](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/oauthproxy.go#L955)) and answers with a relative 302. The browser resolves that against the callback's host, which is the host the user started on.
- One exception: the check for relative URLs refuses any path or query containing `//`, `/./` or `/../` ([`validator.go` L16](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/pkg/app/redirect/validator.go#L16)), `?next=https://…` for example. Such a user lands on `/` instead of the deep link. The shared proxy accepts the whitelisted absolute URL without that check, so it does not have this problem.

**Choice.** Leave it empty and accept the log line, one per sign-in on an out-of-zone host.
- No list could name every out-of-zone host, because they belong to the Applications, not the bootstrap. A suffix wide enough to cover them (`.com`) would let `https://shop.example.com/oauth2/start?rd=https://anything.com/` send a user off-host after sign-in.
- Empty allows only relative paths, which never leave the host the browser is on.
- The cost is the deep-link exception above. It is recorded in `bootstrap/README.md`.

## Chart: three Ingresses and a callback route

`application.login.hostOnlyDomains` lists the custom domains outside `platform.loginCookieDomain` when login is on. The Platform address must be inside the cookie domain, so `baseDomain` is too, and none of these hosts is directly under it. They are always among the `-http01` Ingress's foreign hosts.

- **`<name>-host-login`, new.** It holds their rules, each `/` to the Application, and names `oauth2-proxy-itema-login-host-auth@kubernetescrd`. Traefik gives every router of an Ingress that Ingress's middlewares ([`kubernetes.go` L750](https://github.com/traefik/traefik/blob/v3.7.8/pkg/provider/kubernetes/ingress/kubernetes.go#L750)), so they cannot stay on `-http01` with in-zone hosts. It has a TLS section with the hosts and no secret, like the Platform Ingress.
- **`<name>-http01` keeps every certificate.** Their TLS entries, secret names and the issuer annotation stay there. Their rules stay too, but without `http`. Both halves are needed:
  - **The certificates must not move.** ingress-shim names a Certificate after its secret. It refuses to touch one controlled by another Ingress ([`sync.go` L454](https://github.com/cert-manager/cert-manager/blob/v1.21.2/pkg/controller/certificate-shim/sync.go#L454)), and deletes the ones it controls that leave its Ingress ([L184](https://github.com/cert-manager/cert-manager/blob/v1.21.2/pkg/controller/certificate-shim/sync.go#L184)). The deletion requeues only the old owner ([`ingresses/controller.go` L124](https://github.com/cert-manager/cert-manager/blob/v1.21.2/pkg/controller/certificate-shim/ingresses/controller.go#L124)). So turning login on for a running out-of-zone domain, if its TLS entry moved, could leave it with no Certificate until the 10-hour resync, its Secret still served but no longer renewed.
  - **The rules without `http` keep `-http01` valid.** An Ingress whose only foreign hosts are out of zone would otherwise have none, and Kubernetes refuses an Ingress with neither rules nor a default backend ([validation L358](https://github.com/kubernetes/kubernetes/blob/v1.36.4/pkg/apis/networking/validation/validation.go#L358)). A rule with only a host is valid ([L449](https://github.com/kubernetes/kubernetes/blob/v1.36.4/pkg/apis/networking/validation/validation.go#L449)). Traefik skips it without a word ([`kubernetes.go` L361](https://github.com/traefik/traefik/blob/v3.7.8/pkg/provider/kubernetes/ingress/kubernetes.go#L361)), after loading the Ingress's certificates ([L281](https://github.com/traefik/traefik/blob/v3.7.8/pkg/provider/kubernetes/ingress/kubernetes.go#L281)). The guardrails' Ingress-host policy accepts it: the host is declared. The rules are always listed this way, not only when needed, so there is one shape.
- **`<namespace>-<name>-oauth2`, in `oauth2-proxy`.**
  - **What it routes.** `https://<host>/oauth2/` (Prefix) on each out-of-zone host goes to the Service `oauth2-proxy-host`, port `http`. There is no middleware, so the callback does not go through ForwardAuth.
  - **Its namespace.** It lives there because Traefik routes an Ingress only to Services in its own namespace. `allowExternalNameServices` and `allowCrossNamespace` stay off.
  - **Its name.** The name carries the Environment's namespace, which is unique per Environment. ArgoCD tracks it as the Environment's, so it is deleted with it. The `default` AppProject allows the destination.
  - **What ignores it.** The guardrails bind only to Application namespaces, so they do not see it. Argus files objects by namespace, and no Environment's destination is `oauth2-proxy`.
  - **What it costs.** An Application on such a host can no longer serve `/oauth2/` paths of its own, since this route wins.
- **Router priority.** With no explicit priority, a router's priority is the length of its rule ([`router.go` L244](https://github.com/traefik/traefik/blob/v3.7.8/pkg/server/router/router.go#L244), [`mux.go` L75](https://github.com/traefik/traefik/blob/v3.7.8/pkg/muxer/http/mux.go#L75)). `Host("shop.example.com") && PathPrefix("/oauth2/")` (50 characters) beats the Application's `PathPrefix("/")` (43). cert-manager's challenge `Path(...)` is longer still, so the HTTP-01 challenge still reaches the solver (`76-login-in-zone-domains.md`).
- **The certificate on the callback route.** The Ingress in `oauth2-proxy` names no secret, and the host's Secret is in the Environment's namespace. Traefik puts every Secret an Ingress names into one default store ([`getCertificates`](https://github.com/traefik/traefik/blob/v3.7.8/pkg/provider/kubernetes/ingress/kubernetes.go#L803), [`tlsmanager.go` L129](https://github.com/traefik/traefik/blob/v3.7.8/pkg/tls/tlsmanager.go#L129)). It picks the certificate by SNI before any router runs ([`GetBestCertificate`](https://github.com/traefik/traefik/blob/v3.7.8/pkg/tls/certificate_store.go#L77)). So the `-http01` Ingress's Secret serves the host on every router, the callback's included. The kind test checks this with a real TLS handshake (below).
- **Found while testing that, and older than this change:** Traefik stops loading an Ingress's certificates at the first Secret that does not exist. It logs `Error configuring TLS … secret <ns>/<name> does not exist` and serves its default certificate for every host listed after that one.
  - **In kind:** the in-zone host's Secret on `shop-staging-http01` never exists, since nothing can issue it. So the out-of-zone host's Secret, listed after it, was never loaded. Once both existed, Traefik served `CN=shop-staging.other.test` on `/oauth2/callback`.
  - **On the Platform:** a foreign host whose certificate is not issued yet hides the certificates of the foreign hosts listed after it in `domains`. That is the normal start for every out-of-zone host: the CLI prints the CNAME after the domain is added, and the certificate waits for it. When several are added together, one whose record comes last holds back the others. One whose record is never created hides every host after it for good, including any added later, since the CLI appends to `domains`.
  - **Not fixed here.** It concerns every foreign host, with or without login, and is filed as #163.
- **Sign-in groups.** With `login.groups` and out-of-zone hosts, the Environment also gets `<name>-itema-login-host`: `itema-login-host-auth` with `?allowed_groups=<ids>`, at sync wave -1 like `<name>-itema-login`. `<name>-host-login` names it. `TestLoginGroupsOutsideTheCookieDomainUseTheirOwnHostOnlyMiddleware` requires it to equal `middleware-host.yaml` apart from the query.
- **Still refused:** a Platform address outside `platform.loginCookieDomain`. `application.login.checkDomains`, renamed `application.login.checkPlatformAddress`, keeps only that check.
- `application.login.annotation` now takes the root context and whether the Ingress carries out-of-zone hosts.

## CLI

- **No refusal is left.** The Platform address is always `<name>.<baseDomain>`, and the bootstrap refuses a `baseDomain` outside `cloudflareZone`, so the CLI has nothing to check. `platformrepo.CheckLoginDomains`, `checkDomainsForLogin` and the CLI's pre-check before the Create and Adopt paths are gone.
- **`Result.LoginCallbackHosts`** lists the out-of-zone hosts that the run puts behind login:
  - `app create` (flags, wizard, Create and Adopt): the domains outside the cookie domain, with `--login`.
  - `add-capability --login`: `prod`'s domains already there plus the ones given with it.
  - `add-capability --domain` on an Application with login: the ones added.
  - `--login-group` alone adds none: it changes who gets in, not where they sign in.
- **The closing summary** prints them after the `CNAME` lines, which the same hosts need. It lists each `https://<host>/oauth2/callback`, then:

  ```
  app=$(az ad app list --display-name iidp-oauth2-proxy --query '[0].appId' -o tsv)
  az ad app update --id "$app" --web-redirect-uris $( (az ad app show --id "$app" --query 'web.redirectUris[]' -o tsv; printf '%s\n' <uris>) | sort -u)
  ```

  Then the portal steps, and the error a user sees until it is done (AADSTS50011).
  - `--web-redirect-uris` replaces the whole list, so the command reads the URIs already there and writes them back with the new ones. `sort -u` drops a URI that is already registered.
  - It works in bash and zsh, both of which split the command substitution into one argument per URI. That was checked with a stand-in `az`.
  - The CLI prints the command and never runs it: changing the registration is the Platform admin's call, and the CLI has no Entra access.
- **The Login line** said "sign in once to reach every protected address". It now says one sign-in covers every protected address inside the cookie domain.
- **The wizard** asks the login question whatever the domains, so it no longer reads `platform.yaml` before the summary.

## The trade-off

The host-only cookie reaches no host but the one that set it, so nothing outside the Platform sees it. But every out-of-zone host shares the cookie name and secret. A `__Host-itema_login` captured on one out-of-zone host is accepted on any other until it expires (7 days). Sign-in groups still apply there, since each Environment's Middleware checks them. It is not accepted by the shared proxy, because the name is signed into the value. This is recorded in `bootstrap/README.md`. A cookie secret per proxy, or per host, would close it, and is out of scope (#148).

## Rollout

1. **Release.** The chart change and the bootstrap change ship in the same release.
2. **Bootstrap pin first.** Move the Platform's bootstrap pin to the release. The host-only proxy and `itema-login-host-auth` must exist before any Environment names them. Traefik drops a router whose middleware is missing, which answers 404.
3. **Chart next.** Bump an Environment's chart before giving it login on an out-of-zone domain. A chart from before this release refuses the values, and ArgoCD shows the sync error.
4. **Entra and DNS per host.** Add each host's redirect URI to `iidp-oauth2-proxy` as the CLI prints, and create its CNAME. Without the URI the sign-in stops at AADSTS50011. Nothing else breaks.

Moving iprofil to `iprofil.itema.no` is a separate step after the release.

ADR-0004 still describes the one shared proxy. It records the decision as taken then and is left as it is.

## Tests

- `bootstrap`: `TestSharedOauth2ProxyIsPinned`, `TestHostOnlyOauth2ProxyIsTheSharedOneWithHostOnlyCookies`, with and without a zone, and `TestLoginMiddlewares` for the second Middleware. `TestOauth2ProxyPointsAtThePinnedChartAndPlatformValues` now expects three sources.
- `chart/application`:
  - `refuse-login-domain-outside-cookie-domain.yaml` became `login-domains-outside-cookie-domain.yaml`, which renders. It mixes a wildcard host, an in-zone foreign host and two out-of-zone hosts.
  - The tests check the three Ingresses' hosts, middlewares, TLS entries and the rules without paths, and the `/oauth2/` Ingress's namespace, path, backend, missing middleware and name.
  - They check that nothing of it renders in-zone or without login, the groups Middleware and its equality with the bootstrap's, the base-domain fallback, and the Platform-address refusal.
  - kubeconform runs on the fixture, and with the CRD schemas with groups (9 objects).
- `internal/cli` (`app_login_host_only_test.go`):
  - `app create` with flags, on the Create path and on the Adopt path, plus the wizard, which asks the login question.
  - `add-capability --login` with the domains given together and already present, `--domain` on an Application with login, and the base-domain fallback without a zone.
  - None prints redirect URIs without login, for `--domain` without login, or for `--login-group`.
  - The refusal tests these replace are gone.
- `test/e2e`: `shop-staging` gets `shop-staging.other.test`, outside the fixture zone. `TestBootstrap` checks:
  - its sign-in redirect: the authorize endpoint, `redirect_uri=https://shop-staging.other.test/oauth2/callback`, a `__Host-itema_login_csrf` cookie with no `Domain`, Secure and Path=/, and the state ending in the path;
  - that `/oauth2/callback` on it is answered by `oauth2-proxy-host`, found by a marker in its request log and absent from the shared proxy's, and is neither nginx nor a redirect;
  - that Traefik presents the host's own certificate there, with the host sent as SNI. The test writes a self-signed certificate into every Secret the `-http01` Ingress names, for the reason above;
  - that its ACME challenge bypasses login;
  - that the group Middleware copy is named on `shop-staging-host-login`;
  - that the `/oauth2/` Ingress is gone after the Environment is deleted.

  The in-zone checks are unchanged, and still expect the callback on `auth.app.example.test`. `harness_test.go` checks the new checks against local servers, including a server that presents the host's certificate only from the second handshake on. A reused connection would keep its first certificate, from before Traefik loaded the Secret, so the check opens a new one for every poll. `TestBootstrap` passed locally on kind with Podman (771 s).

Not covered by an automated test: a completed sign-in against the real Entra ID on an out-of-zone host, and a browser's handling of the `__Host-` cookie. Both are checked by hand on the Platform when the first out-of-zone host is turned on.
