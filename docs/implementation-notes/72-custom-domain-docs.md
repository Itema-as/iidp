# #72 Custom domains: only cloudflareZone is automatic

Some docs said a custom domain on any Cloudflare-hosted zone was automatic. The Platform automates DNS for one zone only, `platform.yaml`'s `cloudflareZone`:
- external-dns has a single `domainFilters` entry, that zone (`bootstrap/templates/external-dns.yaml`);
- the CLI classifies any other host as needing a CNAME (`docs/platform-repository.md`).

These now say so:
- `docs/design.md` (the Addresses row and wizard question 6);
- `chart/application/README.md` and `chart/application/values.yaml`;
- the two ClusterIssuers' comments.

The HTTP-01 issuer's comment had the same mistake the other way round. It said that issuer was for zones "not on Cloudflare", but the chart sends every custom domain the wildcard doesn't cover to it, including hosts inside `cloudflareZone`.

## The DNS-01 issuer: comment reworded, no selector

**Choice:** reword the `letsencrypt` ClusterIssuer's comment, and don't add `selector.dnsZones`.

**Why:**
- **The comment was wrong:** it explained the missing selector as "what custom domains on Cloudflare zones need", but no custom domain uses this issuer. Its only Certificate is the Platform's wildcard (`bootstrap/components/tls/templates/certificate.yaml`).
- **A selector changes nothing issued:** it would only matter if a second Certificate asked this issuer for a name outside `baseDomain`, and none does.
- **It isn't free to add:** it would change the solver the live wildcard renews through, and a kind run can't test that, because kind can't complete a DNS-01 challenge.

The comment now states the issuer's single use and why it has no selector. If a second DNS-01 Certificate ever appears, that is the time to add one.

Out of scope, as in the issue: automating more than one Cloudflare zone.
