# #71 The e2e harness's S3 server: versitygw instead of MinIO

Decisions taken while replacing MinIO in the kind e2e harness (`test/e2e/harness.go`). Sources were checked on 2026-09-24: versitygw's documentation and wiki (Context7 `/versity/versitygw`: the Docker, POSIX-metadata and Global-Options pages), its `cmd/versitygw/main.go`, `Dockerfile` and `docker-entrypoint.sh` at tag `v1.8.0`, SeaweedFS's documentation (Context7 `/seaweedfs/seaweedfs`: Quick-Start-with-weed-mini, S3-Credentials, Amazon-S3-API), both projects' GitHub repository metadata and releases, and the registries themselves (ghcr.io's and Docker Hub's tag listings).

## Why MinIO had to go

On 2026-09-24, between 12:45 and 13:04 UTC, quay.io stopped serving `quay.io/minio/minio` and `quay.io/minio/mc` to anonymous clients: pulls answer 401 and the repository API says "Requires authentication". The harness had moved to quay.io in #39 because `docker.io/minio/*` had already done the same (see [`39-final-backup-predelete-hook.md`](39-final-backup-predelete-hook.md)). Every kind e2e run failed in `InstallMinIO` from then on, on every PR and on `main`. The live Platform was unaffected: it uses Hetzner Object Storage, and MinIO was only ever a stand-in the harness installs.

Mirroring the old MinIO images was out of scope, and so was Chainguard's MinIO image, whose free tier is `latest` only and cannot be pinned.

## What the server has to do

The Postgres Capability backs up with CloudNativePG's Barman Cloud plugin, which runs `barman-cloud-*` (boto3 underneath) against the ObjectStore's `endpointURL`. It needs PutObject, multipart upload, ListObjectsV2, GetObject, DeleteObject(s) and HeadBucket, with path-style addressing, over plain HTTP inside the cluster. The chart's ObjectStore names no region, so botocore signs with `us-east-1`. The harness also has to create the bucket (`iidp-backups`, the fixture's `platform.backupsBucket`) before the first Backup, and it has to stay small: the kind node sits near the 2-vCPU runner's CPU request ceiling (#39's notes).

## The candidates

| | versitygw | SeaweedFS |
|---|---|---|
| Licence | Apache-2.0 | Apache-2.0 |
| Current release | `v1.8.0` (2026-09-04) | `4.47` (2026-09-14) |
| Image | `ghcr.io/versity/versitygw` (also Docker Hub `versity/versitygw`), one tag per release, multi-arch index | `chrislusf/seaweedfs` on Docker Hub (tag `4.47`) and `ghcr.io/seaweedfs/seaweedfs` (tag `4.47.0`), one tag per release |
| What runs | one Go binary on Alpine, a gateway translating S3 to a backend; the posix backend stores objects as files | `weed mini` or `weed server -s3`: master, volume server, filer and S3 gateway in one process |
| Creating the bucket | the posix backend treats each top-level directory as a bucket, so `mkdir` is enough | `S3_BUCKET` on `weed mini`, or `weed shell` `s3.bucket.create` against the running server |
| S3 claims | aims at full AWS S3 compatibility; its docs cover multipart limits, checksums and path-style | its Amazon-S3-API wiki lists every operation Barman needs, including multipart, ListObjectsV2, DeleteObjects and HeadBucket |

## Choice: versitygw

**versitygw `v1.8.0`, pinned by tag and digest**: `ghcr.io/versity/versitygw:v1.8.0@sha256:30292fc2eeacc67a36993b01f7a7a5e3361a19cced0e80c1d71cfa2a4b0a2499` (the multi-arch index digest, read from ghcr.io's registry API). The harness pins its kind node image the same way (`bootstrap/versions.yaml`), so the digest follows existing practice.

- **One small process.** versitygw is only a protocol gateway over a directory; SeaweedFS in one container is still a master, a volume server, a filer and an S3 gateway. For a harness that stores a few megabytes of WAL and one base backup, the smaller moving part wins.
- **No client image.** The bucket is a directory, so an init container running the same image creates it with `mkdir -p /data/iidp-backups`, and the server finds it at start. MinIO needed a second image (`mc`) and a Job that waited for the Service to answer; SeaweedFS would need `S3_BUCKET` or a `weed shell` step against the running server.
- **Pulled from ghcr.io.** versitygw publishes to ghcr.io as well as Docker Hub, and the harness pulls from ghcr.io, where public images pull anonymously and Docker Hub's anonymous pull-rate limit does not apply. This does not separate the candidates: SeaweedFS publishes to both too (`ghcr.io/seaweedfs/seaweedfs:4.47.0`).
- **Same credentials.** versitygw's root account takes any key pair (`ROOT_ACCESS_KEY`/`ROOT_SECRET_KEY`), so the fixture's SOPS-encrypted `backups-credentials.enc.yaml` (`iidpe2e`/`iidpe2epassword`, [`42-backups-credentials.md`](42-backups-credentials.md)) did not change.

SeaweedFS stays the fallback if versitygw ever stops working with Barman.

## Shape in the harness

`Cluster.InstallObjectStorage` (was `InstallMinIO`) applies `objectStorageManifest`: a Deployment and a Service, both named `object-storage`, in `iidp-e2e`, on port 7070 (versitygw's default). The fixture's `shop-staging` `platform.objectStorageEndpoint` is now `http://object-storage.iidp-e2e.svc.cluster.local:7070`. The generic name means a future swap of server leaves the fixture alone. The constants were renamed from `MinIO*` to `ObjectStorage*`.

- The server runs `versitygw --port :7070 --region us-east-1 posix /data`. `--region` repeats the default, to make visible that the gateway checks the signing region and that botocore signs with `us-east-1` when the ObjectStore names none.
- `/data` is an emptyDir, as before. The posix backend keeps object metadata (ETags, checksums, multipart state) in extended attributes; the kind node's volume supports them, locally under Podman and on the GitHub runner. versitygw's `--sidecar` option is the documented way out if a filesystem ever does not.
- There is no Job and no `kubectl wait` on one any more: `rollout status` on the Deployment covers the init container too. If the rollout times out, `InstallObjectStorage` prints both containers' logs.

## Resource requests

| Container | CPU request | Memory request | Limits |
|---|---|---|---|
| `versitygw` | 10m | 32Mi | 250m CPU, 256Mi memory |
| `create-bucket` (init) | 10m | 16Mi | 32Mi memory |

MinIO asked for 10m/128Mi, and its `mc` Job for another 10m/32Mi while it ran. A Pod is scheduled on the larger of its init containers' and its regular containers' requests, not their sum, so the init container costs nothing extra: the harness now requests 10m CPU and 32Mi memory for Object Storage, down from 20m and 160Mi while the bucket Job ran.

## Barman against versitygw

Nothing in the chart or the plugin configuration had to change. The ObjectStore still names only `endpointURL`, the `s3Credentials` Secret and gzip compression. The Barman Cloud plugin archived WAL and wrote a base backup through versitygw the first time. The gateway logged no errors, and the bucket held what Barman writes: `shop/staging/shop-staging-db/wals/0000000100000000/...` and `shop/staging/shop-staging-db/base/<backup id>/...`. The gateway's `.sgwtmp/multipart` staging directory appeared inside the bucket, so the base backup went through multipart upload.

Things I had expected might need work and did not:

- **Region.** botocore signs with `us-east-1` when no region is set, and versitygw's default region is `us-east-1`, so signatures matched. A gateway started with a different `--region` would reject every request, which is why the harness spells the flag out.
- **Checksums.** boto3 1.36 and later send CRC checksums by default, which several S3-compatible servers have mishandled; the usual workaround is `AWS_REQUEST_CHECKSUM_CALCULATION=when_required` in the client's environment. Whatever the plugin's boto3 sends by default, versitygw accepted it, so no workaround was needed.
- **Bucket created outside S3.** A directory made with `mkdir` has none of the bucket metadata (ownership, ACL) the gateway writes when it creates a bucket itself. Barman's HeadBucket, listing and writes as the root account worked against it anyway. The gateway runs in single-account mode here ("No IAM service configured, enabling single account mode" in its log), and I did not dig further into why the missing ACL does not matter.

The gateway also creates a `.vgwlocks` directory next to the bucket under `/data`. It holds its lock files and is harmless here.

## Proof

`KIND_EXPERIMENTAL_PROVIDER=podman go test -tags e2e ./test/e2e/... -run TestBootstrap -v -timeout 30m` passed locally in 422 s. `shop-staging`'s scheduled Backup (`shop-staging-db-20260924132359`) reached `completed` against versitygw. On that run, ArgoCD deleted `shop-staging` while the final-backup hook Job was still active, which is the argoproj/argo-cd#29100 race the test tolerates by design (#39's notes), so that run proved the hook ran but not its Backup. The CI run on the pull request is the other proof. The test's assertions are unchanged.
