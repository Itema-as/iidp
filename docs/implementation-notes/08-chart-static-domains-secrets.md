# #8 Chart: Static site Kind, custom domains and secrets

Questions that came up while adding the Static site Kind, custom domains and secrets to `chart/application`, and the answer chosen for each.

## Where does the wildcard certificate live now?

#6 referenced the wildcard certificate as a Secret in the Environment's namespace (`platform.wildcardTLSSecret`) and noted that if the bootstrap made it Traefik's default certificate instead, the `secretName` could be dropped later. That is now the plan: the bootstrap holds the `*.<baseDomain>` certificate in a `TLSStore` named `default` in Traefik's namespace, and Traefik serves it for any TLS host that names no certificate of its own.

Chosen: the Platform address's `tls` entry lists the host and sets no `secretName`; `platform.wildcardTLSSecret` is removed from `values.yaml`, the README and the `prod-large.yaml` fixture, and the assertion in `chart_test.go` now checks that no `secretName` is set. Nothing has to be replicated into Application namespaces any more. Coordination point for the bootstrap ticket: the `TLSStore` must be named `default`, and Traefik's `websecure` entrypoint must redirect from `web`; the chart routes only `websecure`.

## What does a Static site change?

Chosen: `application.port` is 80 for `kind: static-site` and `.Values.port` otherwise, so the container port, the Service, the Ingress backends and the probes all follow from that one helper. A Static site's container gets no `PORT` variable: nginx does not read one, and injecting it would suggest the port is configurable. `port` in the values is ignored for a Static site rather than refused, because `3000` is both the default and a legitimate Web service value, so the chart cannot tell "set" from "left alone". The probes hit `probe.path` (default `/`) on port 80, which nginx answers for any site with an `index.html`; there is no nginx-specific probe path, because nothing about nginx makes `/` a worse choice than for a Web service.

The `env` block is only rendered when there is something in it (`PORT` or plain env), so a Static site without `env` has no empty `env:` key.

## Does the `env.PORT` refusal still make sense?

Chosen: kept for both Kinds. For a Web service the reason is unchanged (a duplicate would be silently ambiguous). For a Static site, a `PORT` in `env` would say the container listens somewhere other than 80, which the chart does not honour, so refusing it is more honest than passing it through to an nginx that ignores it. One rule ("`PORT` is the Platform's") is also easier to state than a per-Kind one.

## Which hosts are covered by the wildcard?

`*.<baseDomain>` matches exactly one label. A custom domain `butikk.app.itma.no` is covered; `test.shop.app.itma.no` is not, and neither is `shop.itma.no`, which sits on the Platform's own Cloudflare zone but outside the base domain. Whether external-dns can create the record (a Cloudflare zone) and whether the wildcard serves the host are independent questions; the chart only answers the second.

Chosen: a host is a wildcard host when it ends in `.<baseDomain>` and the part before that suffix contains no dot. Everything else is a foreign host and gets its own certificate. Two labels under the base domain could in principle be covered by a second wildcard, but the bootstrap issues one.

## Where do the foreign hosts go?

cert-manager's ingress-shim (cert-manager docs, "Securing Ingress Resources") watches for `cert-manager.io/cluster-issuer` on an Ingress and ensures a `Certificate` per `tls` entry, named after its `secretName`, with the entry's hosts as SANs. It acts on the whole object it finds the annotation on. If the foreign hosts shared the Platform Ingress, the annotation would sit beside the wildcard hosts' `tls` entry; that entry has no `secretName`, so ingress-shim would skip it today, but the two concerns would be one mis-edit apart.

Chosen: a second Ingress, `<fullname>-http01` (`shop-http01`, `shop-staging-http01`), rendered only when there is at least one foreign host. It carries the same Traefik annotations, the issuer annotation, one `tls` entry per host with `secretName: <fullname>-<host with dots replaced by dashes>-tls`, and one rule per host routing `/` to the same Service. One entry per host means one Certificate per host, so a domain that fails validation does not hold up the others. The Platform Ingress never carries the issuer annotation, and the tests assert that.

The secret name uses the Environment's object name (`<name>` or `<name>-staging`) rather than the bare Application name, so prod and staging of one Application can list the same foreign host shape in one namespace without their Certificates colliding.

## Which issuer, and what does the bootstrap owe the chart?

Chosen: `platform.httpIssuer`, default `letsencrypt-http01`. The bootstrap creates a `ClusterIssuer` of that name with an ACME HTTP-01 solver of `ingress.ingressClassName: traefik` (the form the current cert-manager docs show), next to the DNS-01 issuer that produces the wildcard. Coordination points, for the bootstrap ticket:

- The `ClusterIssuer` name `letsencrypt-http01`, or the Platform repository overrides `platform.httpIssuer` in every values file.
- The solver Ingress cert-manager creates for a challenge carries no Traefik annotations, so Traefik attaches it to both entrypoints. With the bootstrap's HTTP-to-HTTPS redirect on `web`, the challenge request for `http://<host>/.well-known/acme-challenge/<token>` is answered with a redirect to `https://<host>/...`. The cert-manager docs consulted (HTTP-01 configuration, troubleshooting) only require the challenge URL to be reachable from the public internet; Let's Encrypt's documentation of the HTTP-01 challenge states that its validator follows redirects, including to HTTPS, and does not validate the certificate it meets there, and cert-manager's own self-check follows redirects too. The default certificate it meets is the wildcard, which does not match a foreign host, but that is fine for the validator. So the redirect should not need an exemption; the bootstrap ticket should still verify one foreign-domain issuance end to end and, if it fails, exempt the `/.well-known/acme-challenge/` prefix from the redirect rather than change the chart.

## What is refused?

Chosen, all with a `fail` naming the value: a domain that is not a valid DNS hostname (lowercase labels of letters, digits and dashes, dot-separated, at least two labels, at most 253 characters; uppercase is refused rather than lowercased, so a duplicate cannot hide behind case), a domain listed twice, a domain equal to the Environment's own Platform address (it is always served; listing it would render the host twice), and a foreign domain so long that `<fullname>-<host>-tls` would exceed the 253 characters a Secret name may have (cert-manager names the Certificate after the secret, so a longer name would fail at issuance instead of at render). A domain equal to the other Environment's Platform address is not refused; it would render as a wildcard host and Traefik would then see the same host on two Ingresses, which is a mistake the CLI is better placed to prevent because only it knows both Environments exist.

## Secrets

Chosen: `secrets` is a list of Secret names; each becomes an `envFrom.secretRef` on the container, rendered only when the list is non-empty. No validation beyond what Kubernetes does: a Secret that does not exist fails the Pod at start with a clear event, and the CLI writes both the name and the Secret. The migration Job (`templates/migration-job.yaml`) does not exist on this branch; the Postgres ticket that adds it should give its container the same `envFrom` block, because a migration command may need the same credentials as the Application.

## What changed in the existing tests and CI?

`chart_test.go` changed in two places only: the wildcard assertion (`secretName` must be absent instead of equal to a value) and the removal of the `static-site.yaml` refusal case, which this ticket turns into a rendering fixture. The new assertions live in `static_site_test.go` and `domains_test.go`, which reuse the helpers of `chart_test.go` and validate their own fixtures with kubeconform.

The `Chart` job in `.github/workflows/ci.yaml` lints a fixed list of fixtures (`prod-small`, `staging-medium`, `prod-large`); the new fixtures are linted locally and rendered under kubeconform by the tests, but the workflow's list was not extended because the workflow is outside this ticket's files. Follow-up: make that loop lint every rendering fixture, or read the list from one place.
