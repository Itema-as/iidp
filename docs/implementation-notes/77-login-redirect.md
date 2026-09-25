# #77 / #78 Itema login redirects straight to Entra ID

Decisions taken while fixing the Itema login sign-in, reported twice: #77 (the unstyled sign-in page) and #78 (from `iprofil`: nobody can sign in). Sources were checked on 2026-09-25: oauth2-proxy's Traefik integration page (`configuration/integrations/traefik`) and its page-template options (`configuration/overview`), and oauth2-proxy's source at tag `v7.15.3`, the appVersion `bootstrap/versions.yaml` pins: `oauthproxy.go` (the proxy handler's unauthenticated branch, `OAuthStart`, `isAjax`), `pkg/app/redirect/director.go` (`GetRedirect`) and `pkg/app/pagewriter/sign_in.html`; and Traefik's source on `master`: `pkg/middlewares/auth/forward.go` (what ForwardAuth does with a non-2xx answer, and the `X-Forwarded-*` headers it sets) and `pkg/middlewares/customerrors/custom_errors.go` (`{url}`).

## What was wrong

#18 chose the two-middleware shape from oauth2-proxy's Traefik docs: ForwardAuth against `/oauth2/auth`, which answers 401, and an `errors` middleware that replaces the 401 with the body of `/oauth2/sign_in?rd={url}`, status rewritten to 302. On the live Platform (`iprofil.app.itma.no`) that 302 had no `Location` header. An `errors` middleware serves another page's body; it does not redirect. The browser therefore rendered oauth2-proxy's sign-in page at the Application's own URL. The page's links are relative (`/oauth2/static/css/bulma.min.css`, the form's `/oauth2/start`), so they went to the Application's host, through the same two middlewares, and came back as the same page: no styling, and a button that only reloads it.

The docs' version of that shape works because it also routes `Host(<app>) && PathPrefix(/oauth2/)` to oauth2-proxy without the middlewares. The chart never had that route.

The kind e2e passed throughout because `Cluster.CheckRedirect` accepted any 3xx and only logged `Location`.

## Choice: ForwardAuth against the root address, with `skip-provider-button`

`itema-login-auth` now points at oauth2-proxy's root address, and oauth2-proxy runs with `skip-provider-button`. The `errors` middleware is out of the chart's annotation.

- For the root address, oauth2-proxy runs its proxy handler. With a valid session it serves the alpha-config static upstream (200), and Traefik forwards the request to the Application with the `X-Auth-Request-*` headers, as before.
- Without one, `oauthproxy.go` answers a request that accepts `application/json` with a JSON 401. Anything else gets `doOAuthStart` when `SkipProviderButton` is set, and the sign-in page with **403** when it is not. That 403 is what #18 saw when it first tried the root address and put down to the static upstream. It was the sign-in page.
- `doOAuthStart` sets the CSRF cookie (for the cookie domain, `.<baseDomain>`, so the callback on `auth.<baseDomain>` can read it) and redirects to Entra ID. The URL to come back to is `GetRedirect`'s: no `rd` parameter or `X-Auth-Request-Redirect` header here, so in reverse-proxy mode it is built from `X-Forwarded-Proto`, `-Host` and `-Uri`, which ForwardAuth sets from the original request. It is carried in the OAuth `state` as `<csrf hash>:<URL>` (no `--encode-state`), and validated against `whitelist-domain`.
- `forward.go` copies every header of a non-2xx ForwardAuth answer except hop-by-hop ones onto the client's response, sets `Location` from it (an absolute one unchanged), and writes its status. The browser gets oauth2-proxy's 302 with its `Location` and `Set-Cookie`. `X-Forwarded-Uri` is the request URI (path and query), `X-Forwarded-Host` the request's host, and `X-Forwarded-Proto` `https` on the TLS entrypoint.

This is the docs' other shape ("ForwardAuth with Static Upstreams Configuration"), with the provider button skipped so the happy path never shows an oauth2-proxy page: Application, Entra ID, callback, Application. #77 chose that over restyling the page or replacing its templates.

Rejected: keeping the two middlewares and adding the `/oauth2/` route #78 proposed. It would work (`{url}` is the full original URL, scheme and host included), but it needs a route to oauth2-proxy in every protected Environment's namespace, which an Ingress cannot name across namespaces: an `ExternalName` Service per Environment, or a Traefik `IngressRoute` with cross-namespace references allowed. It also keeps a round trip through `/oauth2/sign_in` on the Application's host, and the page would still be shown unless the provider button were skipped anyway. The root address needs neither, and no chart change beyond dropping a name.

`footer: "-"` removes "Secured with OAuth2 Proxy version …" from the pages oauth2-proxy still serves (errors, sign-out).

## Environments on older charts

Application charts up to 0.2.0 name both middlewares, `itema-login-errors` in front of `itema-login-auth`, and an Environment keeps its chart until someone bumps it (`iprofil` pins 0.2.0). Traefik refuses a router whose middleware does not exist, so deleting `itema-login-errors` would take those Environments off the air. It stays, unchanged. In front of the new `itema-login-auth` it no longer matches a browser's request, which now gets a 302, so those Environments get the same redirect without a chart upgrade. A JSON-only request still gets its 401 replaced with `/oauth2/sign_in`, as before. The Middleware's comment says to delete it once nothing in the Platform repository pins an older chart.

## Tests

- `test/e2e`: `CheckRedirect` became `CheckSignInRedirect`. It requests a path with a query and requires a 3xx whose `Location` is the provider's authorize endpoint (in kind, the fixture's never-resolved `https://oauth2-proxy-entra.invalid/authorize`), with `redirect_uri` `https://auth.<baseDomain>/oauth2/callback`, a `state` ending in the original `https://` URL with path and query, and a `_oauth2_proxy_csrf` cookie for the base domain. `TestBootstrap` runs it against `shop-staging`, and against a temporary Ingress in the same namespace that carries the 0.2.0 annotation. `harness_test.go` (no build tag, so it runs in the Go job) checks `CheckSignInRedirect` itself against a local TLS server: it fails on a 302 without `Location` (what `iprofil` served), on a lost return URL, and on a 403 sign-in page.
- `bootstrap`: `skip-provider-button` and `footer` in oauth2-proxy's values; `itema-login-auth` at the root address; `itema-login-errors` still defined.
- `chart/application`: the annotation names only `itema-login-auth`, and every middleware it names is defined in `bootstrap/components/oauth2-proxy-login/middleware.yaml`.

Not covered by an automated test: a completed sign-in against the real Entra ID, which kind cannot reach. It is checked by hand on `iprofil` once a release carrying this is on the Platform.
