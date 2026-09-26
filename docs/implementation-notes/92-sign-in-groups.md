# #92 Sign-in groups for Itema login

Itema login can be restricted to the members of listed Entra groups, per Application: `--login-group <object-id>` on `iidp app create` and `iidp app add-capability`, written as `login.groups` into every Environment. With no group, any Itema user gets in, as before. No Entra registration per Application is created; this replaces `docs/design.md`'s "per-app Entra registrations" (spec #88).

Sources were checked on 2026-09-26:

- oauth2-proxy's source at tag [`v7.15.3`](https://github.com/oauth2-proxy/oauth2-proxy/tree/v7.15.3), the appVersion `bootstrap/versions.yaml` pins (chart `10.7.0`). The files are linked below.
- oauth2-proxy's Microsoft Entra ID provider page at the same tag, [`docs/docs/configuration/providers/ms_entra_id.md`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/docs/docs/configuration/providers/ms_entra_id.md).
- Traefik's source at [`v3.7.8`](https://github.com/traefik/traefik/tree/v3.7.8), the image `bootstrap/versions.yaml` pins for k3s's Traefik.
- Microsoft's [app manifest reference](https://learn.microsoft.com/en-us/entra/identity-platform/reference-microsoft-graph-app-manifest) (`groupMembershipClaims`), [Configure group claims](https://learn.microsoft.com/en-us/entra/identity/hybrid/connect/how-to-connect-fed-group-claims) (the 200-group limit) and the [`az ad app`](https://learn.microsoft.com/en-us/cli/azure/ad/app?view=azure-cli-latest) reference (`az ad app update --set groupMembershipClaims=...`).

## The Middleware: the root address with `allowed_groups`, not `/oauth2/auth`

**Question.** The spec says a per-Application ForwardAuth Middleware pointing at the shared oauth2-proxy's `/oauth2/auth?allowed_groups=…`. #77 moved the shared middleware off `/oauth2/auth` to the root address, because `/oauth2/auth` answers an unauthenticated request with a bare 401 and never redirects (`77-login-redirect.md`). Pointing a group-restricted Environment at `/oauth2/auth` would bring that bug back: an unauthenticated browser would get a 401, not Entra ID.

**Finding.** In v7.15.3 the root address checks `allowed_groups` too.

- The router sends `/oauth2/auth` to `AuthOnly` and everything else, `/` included, to `Proxy` ([`oauthproxy.go` L331, L339](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/oauthproxy.go#L331)).
- [`Proxy`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/oauthproxy.go#L1041) calls `getAuthenticatedSession`. With a valid session it runs [`authOnlyAuthorize`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/oauthproxy.go#L1180), the same check `AuthOnly` runs, and answers `http.Error(rw, "Forbidden", 403)` when it fails. Without a session it does what #77 relies on: `doOAuthStart` with `skip-provider-button`, a 302 to Entra ID.
- `authOnlyAuthorize` runs [`checkAllowedGroups`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/oauthproxy.go#L1245), which reads `allowed_groups` from the request's own query with [`extractAllowedEntities`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/oauthproxy.go#L1204). That splits each value on commas, so `allowed_groups=a,b` is two groups. It passes when any group of the session is listed, and when the parameter is missing or empty. The comparison is an exact string match, so the chart and the CLI lowercase the ids, which is the form Entra puts in the `groups` claim and Graph returns.
- The 403 does not touch the session. `getAuthenticatedSession` clears the cookie only when the provider's own `Authorize` or the email check fails ([L1142](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/oauthproxy.go#L1142)), and neither is configured here. So a signed-in user outside the groups gets a 403 on every request, and never a redirect or a loop.

**Choice.** Each Environment with groups gets a Middleware `<fullname>-itema-login` in its own namespace, which is the shared `itema-login-auth` with `?allowed_groups=<ids>` on the address:

```yaml
forwardAuth:
  address: "http://oauth2-proxy.oauth2-proxy.svc.cluster.local/?allowed_groups=0f3b6a4e-...,6e1d2c3b-..."
  trustForwardHeader: true
  authResponseHeaders: [X-Auth-Request-User, X-Auth-Request-Email, X-Auth-Request-Preferred-Username, X-Auth-Request-Groups]
```

Both Ingresses (`ingress.yaml` and `ingress-http01.yaml`, through `application.login.annotation`) name `<namespace>-<fullname>-itema-login@kubernetescrd` instead of the shared one. The namespace is `.Release.Namespace`, which ArgoCD sets from the Environment Application's destination. `chart/application/login_test.go` reads `bootstrap/components/oauth2-proxy-login/middleware.yaml` and requires the Environment's `forwardAuth` to equal it in every field except the query on the address, so the two cannot drift apart. The Middleware has `argocd.argoproj.io/sync-wave: "-1"`, because Traefik drops a router whose middleware does not exist yet. Adding groups to a running Environment would otherwise answer 404 for a moment. Without groups nothing changes: no Middleware, and the same annotation as before.

**Rejected.**

- `/oauth2/auth?allowed_groups=` alone. It never redirects, which is #77's bug.
- Chaining two middlewares: the shared one for the redirect, then `/oauth2/auth?allowed_groups=` for the groups. It would work, but it asks oauth2-proxy twice per request for what one call answers.
- `/oauth2/auth` with Traefik's `authSigninURL` ([`forward.go` L264](https://github.com/traefik/traefik/blob/v3.7.8/pkg/middlewares/auth/forward.go#L264)), which turns a 401 into a redirect to a fixed URL. The URL is static, so the page the user asked for would be lost.

## Traefik forwards the query

[`forward.go` L166](https://github.com/traefik/traefik/blob/v3.7.8/pkg/middlewares/auth/forward.go#L166) builds the auth request with `http.NewRequestWithContext(ctx, method, fa.address, nil)`: the address as it is, query included. The original request's path and query go only into `X-Forwarded-Uri` ([`writeHeader`](https://github.com/traefik/traefik/blob/v3.7.8/pkg/middlewares/auth/forward.go#L418)). oauth2-proxy uses that header to build the return URL, but `checkAllowedGroups` reads `req.URL`, the auth request's own URL. A visitor therefore cannot add or change `allowed_groups`. The one way to change the query is the values file, and the chart refuses any group that is not a GUID, so a value like `<guid>&allowed_emails=x` cannot add a second parameter (`refuse-login-group-not-a-guid.yaml`).

## How the `entra-id` provider fills the session's groups

- **From the ID token's `groups` claim.** At the callback, the OIDC provider builds the session from the ID token's claims with [`buildSessionFromClaims`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/providers/provider_data.go#L262), which takes `Groups` from the `groupsClaim` claim. The bootstrap already sets `groupsClaim: groups`. If the ID token lacks the claim, the claim extractor tries the profile URL, and Entra's userinfo endpoint has no groups either.
- **The overage case, from Graph.** Entra puts at most 200 groups in a JWT. Above that it leaves `groups` out and adds `_claim_names.groups` ([Microsoft: Configure group claims](https://learn.microsoft.com/en-us/entra/identity/hybrid/connect/how-to-connect-fed-group-claims)). The Entra provider's [`EnrichSession`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/providers/ms_entra_id.go#L62) runs after the callback ([`enrichSessionState`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/oauthproxy.go#L1002)). It checks for that key ([`checkGroupOverage`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/providers/ms_entra_id.go#L210)) and then pages through `https://graph.microsoft.com/v1.0/me/transitiveMemberOf?$select=id&$top=100` with the session's access token ([`addGraphGroupsToSession`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/providers/ms_entra_id.go#L231)). If Graph refuses, the error is logged and the session gets no groups, so that user gets a 403 on group-restricted Applications and can still use the others.
- **Nested groups count** both ways: the claim includes them (Microsoft, same page), and `transitiveMemberOf` is transitive.
- **Permissions.** The provider page says the Graph fallback needs the delegated `User.Read`, and that with `openid` and `User.Read` admin-consented "group overage works with `openid` scope only". The wizard's registration has `User.Read` and `GroupMember.Read.All`, both admin-consented (`scripts/bootstrap-wizard.sh`, `az_create_entra_app`), and oauth2-proxy asks for `openid email profile`. No scope change.
- **When.** Groups are read at sign-in only. With `cookie-refresh` at 0 (the default, `76-login-in-zone-domains.md`) the session is never refreshed. There would be nothing to refresh with anyway: Entra issues a refresh token only for `offline_access`, which is not asked for. So a user added to a group gets in at their next sign-in, and a user removed keeps access until their session ends, within `cookie-expire` (7 days). `/oauth2/sign_out` on `auth.<baseDomain>` starts over. Turning on refresh (with `offline_access`) would shorten that, but it changes every protected Application's sessions and makes the cookie bigger. It is left for later: [`RefreshSession`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/providers/ms_entra_id.go#L143) re-reads the claim from the new ID token but does not repeat the overage check.

## The Entra registration needs `groupMembershipClaims`

**Finding: yes.** Without it Entra puts no `groups` claim in the ID token, so every session has no groups and a group-restricted Application refuses everyone. The provider page says to "enable *groups claims* in the App Registration", with `group_membership_claims = ["SecurityGroup"]` in its Terraform example. Microsoft's manifest reference lists the values: `None`, `SecurityGroup` (security groups and Entra roles), `ApplicationGroup` (only groups assigned to the application), `DirectoryRole`, `All` (adds distribution groups).

**Choice.** `SecurityGroup`. `ApplicationGroup` would need every group assigned to the `iidp-oauth2-proxy` enterprise app before it works, an admin step per group that the CLI cannot see. `All` adds distribution lists, which makes tokens bigger and the 200-group overage more likely. It gains nothing, since sign-in groups should be security groups. The bootstrap wizard now sets it:

- With `az`, `az_create_entra_app` runs `az ad app update --id <appId> --set groupMembershipClaims=SecurityGroup` (the form the `az ad app update` reference shows) for `iidp-oauth2-proxy` only. A failure is a warning plus a manual step, like the permission steps. ArgoCD's registration is unchanged, because Dex reads groups from Graph itself.
- Without `az`, the manual instructions add "Token configuration: Add groups claim, Security groups, ID token as Group ID".

The live Platform's registration was made before this change, so it needs the setting by hand. That is a manual step for the rollout ticket, #96 (commented there).

## Rollout (manual step for #96)

1. Set the groups claim on the existing `iidp-oauth2-proxy` registration **before** the Platform's bootstrap pin moves to the release carrying #92: `az ad app update --id <iidp-oauth2-proxy appId> --set groupMembershipClaims=SecurityGroup`, or in the Entra admin center, App registrations > iidp-oauth2-proxy > Token configuration > Add groups claim > Security groups (ID: Group ID). Doing it before the bump costs nothing: oauth2-proxy already has `groupsClaim: groups` and only passes the groups on in `X-Auth-Request-Groups`. Doing it first also means the fresh sign-in everyone makes after #76's cookie rename, which comes in the same release, already carries groups.
2. Check that `User.Read` is still admin-consented on the registration (Entra admin center > API permissions: "Granted for Itema"), for users in more than 200 groups.
3. If the step comes after the bump, a user signed in before it has no groups in their session and gets a 403 from a group-restricted Application until they sign in again: `https://auth.app.itma.no/oauth2/sign_out`, then reload.

## CLI

- **`--login-group` needs login.** The issue says it "implies `--login` and is refused without it when login isn't enabled". Read as one rule, that means the groups only mean something with login on, and the flag is refused when login is neither on already nor turned on in the same run. `app create --login-group X` without `--login` is therefore refused ("sign-in groups need Itema login: give --login with --login-group"), and so is `add-capability --login-group X` on an Application without login unless `--login` is given too. Turning login on is a separate, visible decision. The alternative, turning login on silently, was rejected: on `add-capability` it would lock every non-member out of a public Application in the same run that was meant to adjust groups.
- **Only the shape is checked.** `platformrepo.NormalizeLoginGroups` trims and lowercases each id and requires a GUID. It refuses an id given twice, compared lowercased. The refusal says where to find a group's object id. The CLI has no Entra access, so a well-formed id of no group lets nobody in. The chart checks the same.
- **Replace, and remove with `--login-group ''`.** `add-capability --login-group` replaces the list in every Environment; to add a group, list the old ones too. A single empty value clears the list (`login.groups: []`), and every Itema user gets in again. An empty value next to ids is refused, because it would otherwise be dropped without a word. A separate flag (`--no-login-groups`) was the alternative. It would be one more flag for the same list, and the issue suggested the empty value. As with every Capability, a request that changes nothing is refused as already present: the same groups again, or `''` on an Application without groups. `--login --login-group` on an Application that already has login keeps the existing "already enabled" refusal, and adds a hint to give `--login-group` alone.
- **What is written.** `login.groups` in every Environment, always present, `[]` included, the way `domains: []` is. `render.SetLoginGroups` edits in place and keeps comments and other keys. `render.CopyValuesForStaging` needs no change: a staging Environment added later copies `prod`'s groups. The commit subject names the Capability `login-group`.
- **Wizard.** Right after question 7, whenever login is on (answered yes, or `--login` given) and `--login-group` was not given, the wizard asks "Sign-in groups [none]", comma-separated. It first prints how to find a group's object id: the group's Overview page in the Entra admin center, or `az ad group show --group <name> --query id -o tsv`. A bad answer is asked again with the reason. The summary and the closing output list the groups, or say every Itema user gets in.

## Guardrails (#90)

The Middleware is a `traefik.io/v1alpha1` object in the Application's namespace, created by the Environment's ArgoCD Application like the rest of the chart. The four admission policies spec #88 gives #90 cover images, limits, Service types and Ingress hosts in Application namespaces, not Middlewares, and the Middleware carries no hosts and no pod spec. #90 had not landed when this was written. A policy that restricts which objects an Application namespace may hold must keep allowing a `Middleware` there.

## Tests and verification

- `chart/application`: no groups renders no Middleware and the shared annotation on every Ingress (`login-enabled.yaml`, `login-custom-domains.yaml`, and `groups` set to `[]` and `null`). `login-groups.yaml` renders the Middleware with the lowercased ids, and both Ingresses name it. Its `forwardAuth` is the bootstrap's apart from the query. There are `refuse-*` fixtures for groups without login, an id that is not a GUID (one carrying `&allowed_emails=`) and a duplicate that differs only in case, plus a string instead of a list. kubeconform validates the fixture with the datreeio CRD catalogue for the Middleware. The existing unreleased tests pick up the new fixtures: nothing renders without an image, and every refusal is the same with and without one.
- `internal/render`: `SetLoginGroups` writes, replaces, clears and reports no change. A staging copy keeps the groups. `ExampleValues` now shows `groups: []`.
- `internal/cli` (`app_login_groups_test.go`, through `cli.RunWith`): the groups written in order and lowercased in both Environments, and `groups: []` without them. The `app create` refusals: without `--login`, not a GUID, a query injection, a duplicate, and an empty value next to an id. The refusal text says where to find the id. On `add-capability`: the list replaced in every Environment, cleared with `''`, set together with `--login`, and copied to a new staging. Its refusals: without login, clearing without login, not a GUID, the same groups, clearing none, and `--login` again. Nothing is committed on any refusal. The wizard asks the question after login, asks again on a bad id, and shows the groups in its summary; without login it never asks.
- `test/wizard/run.sh`: the manual registration instructions mention the groups claim only for oauth2-proxy, and the dry-run `az` path runs `az ad app update --set groupMembershipClaims=SecurityGroup` only when asked.
- `test/e2e`: the fixture's `shop-staging` has a sign-in group. `TestBootstrap` requires both its hosts to still redirect an unauthenticated request to sign-in, with the return URL and the CSRF cookie, now through the Environment's own Middleware. It also requires the Middleware to exist with `allowed_groups` on its address and both Ingresses to name it. It passed locally on kind with Podman (`TestBootstrap`, 461 s).
- **By hand, against the real oauth2-proxy binary.** oauth2-proxy `v7.15.3` (darwin-arm64 release) ran with the bootstrap's flags and alpha config: `entra-id`, `groupsClaim: groups`, static upstream, `skip-provider-button`, `reverse-proxy`, `cookie-name`, `cookie-domain`, `whitelist-domain`. Its endpoints pointed at a throwaway OIDC issuer that signs ID tokens with an Entra-shaped `iss` and a `groups` claim. Requests were sent the way ForwardAuth sends them (the Middleware's address as the URL, `X-Forwarded-Proto/-Host/-Uri`):

  | Request | Answer |
  |---|---|
  | `/?allowed_groups=<member>`, no session | 302 to the issuer's authorize endpoint, CSRF cookie set, state ending in the original `https://shop.app.example.test/orders?page=2` |
  | the callback | 302 back to `https://shop.app.example.test/orders?page=2`, session cookie set, session groups from the claim |
  | `/`, signed in (shared middleware) | 200, `X-Auth-Request-Groups` set |
  | `/?allowed_groups=<member>` | 200 |
  | `/?allowed_groups=<other>,<member>` | 200 |
  | `/?allowed_groups=<other>` | 403 `Forbidden`, no `Location`, no `Set-Cookie` |
  | `/oauth2/auth?allowed_groups=<other>` (for comparison) | 403 |

  The issuer's `iss` had to be `https://login.microsoftonline.com/<tenant>/v2.0`. With any other issuer, the callback answers 403 "Session validation failed", because the Entra provider's [`ValidateSession`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.3/providers/ms_entra_id.go#L83) requires an Entra issuer. That is irrelevant on the Platform, where Entra issues the token.

**Not verified.** A real Entra sign-in with the groups claim, the Graph overage path (`graph.microsoft.com` is hard-coded in the provider, so a local run cannot fake it), a signed-in user's 403 through Traefik in kind (kind cannot complete a sign-in), whether Microsoft 365 groups appear in the claim with `SecurityGroup` (the docs list security groups and Entra roles; a security-enabled Microsoft 365 group may or may not be included), and how big the session cookie gets for a user with many groups (oauth2-proxy splits it into several cookies of 4 KB). #96 step 8 checks a member and a non-member on `hello` by hand.
