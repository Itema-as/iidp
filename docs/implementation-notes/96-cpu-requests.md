# #96 CPU requested at a fifth of its limit

Found during the Phase 2 rollout (#96, step 11). The first Preview Environment on the live Platform, `hello-pr-2`, got its database, but its migration Pod stayed Pending for ten minutes: `0/1 nodes are available: 1 Insufficient cpu`. The CPX22 node had no CPU left to book.

## Why the node was full

Every size requested exactly its CPU limit (small 250m), and so did the Postgres instance (250m). An Environment with Postgres therefore booked 500m of the node's 2 vCPU whether it was busy or idle. Its migration Job booked another 250m while it ran. hello-prod, hello-staging and iprofil-prod booked about 1.25 vCPU, the Platform's components most of the rest, and the preview's database took the last free slice. Actual CPU use was a small fraction of what was booked. These Applications sit idle most of the time.

## The choice

- **Book less CPU, keep the limits.** Chosen. Each size's CPU request is a fifth of its limit: small 50m/250m, medium 100m/500m, large 200m/1. The Postgres instance is 50m/250m. The migration Job and Scheduled tasks follow, since they use the same sizes. Memory stays requested at its limit. Memory can't be taken back from a container that uses it, so booking less of it than the limit would let the node overcommit into OOM kills. CPU is compressible: when several containers are busy at once, each is throttled to its share instead of failing.
- **Resize to CPX32** (4 vCPU / 8 GB, about €53 instead of €29 a month). Not chosen for now. With CPU booked at its limit, the bigger node fills up again at about twice today's size, still mostly idle.
- **Both.** Left as the next step when memory, which is still booked in full, runs out.

## Consequences

- Pods are Burstable instead of Guaranteed. With memory requested at its limit, a container never uses more memory than it requested, so node-pressure eviction still ranks it last.
- A burst of work in several Environments at once now slows them all down, instead of each having its own slice. On a Platform of internal tools, that trade is worth the room.
- The CPU limits don't change, so no Environment can use more CPU than before.
- Environments keep the old requests until their `application.yaml` names a chart version with this change. The CLI writes `platform.yaml`'s `chartVersion` into new Environments.

## Proof

`IIDP_REQUIRE_CHART_TOOLS=1 go test ./chart/...` asserts the request and the limit separately for each size, for Postgres, for a Static site, for a Preview Environment and for a Scheduled task.
