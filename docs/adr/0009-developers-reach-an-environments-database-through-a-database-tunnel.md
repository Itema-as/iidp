---
status: accepted
date: 2026-10-06
---

# Developers reach an Environment's database through a Database tunnel

A developer sometimes needs to look at, or fix, the data in one of their Application's Environments. Until now nothing outside the cluster could reach a database: the node's firewall opens only 22, 80 and 443, and only the Platform admin holds a kubeconfig.

We decided that a developer reaches an Environment's database from their own machine through a **Database tunnel**: `iidp app db connect` listens on `127.0.0.1` and carries each local connection over a WebSocket to the tunnel at `db.<baseDomain>`, on 443. Who may connect follows their permission on the Application repository, which the tunnel asks GitHub for with the developer's own `gh auth` token, on every connection. Each Environment's levels (`postgres.access`, #158) name the lowest permission that may connect read-write and read-only, and the tunnel logs in to the database itself as that level's role.

## Decision

- **Access follows Application repository permission.** The tunnel reads the Application's binding the way the Deploy gate's status endpoint does (ADR-0005, ADR-0007), with one check both share (`internal/repoaccess`), and asks GitHub for the developer's permission on the bound repository: `pull`, `push`, `maintain` or `admin`. It compares that with the levels on the Environment's `Cluster`, which are what is deployed rather than what the Platform repository says, and chooses `<app>_write` or `<app>_read`. GitHub stays the one place where a developer's access to an Application is granted and taken away, and the check never drifts from it.
- **A separate component.** The tunnel is its own binary, image and bootstrap component (`cmd/iidp-db-tunnel`, `bootstrap/components/db-tunnel`), in its own namespace with its own ServiceAccount. It is not part of the Deploy gate's service. The gate holds the `iidp-deploy` App's key, which can rewrite the whole Platform repository, and is the single write path for CI, so it stays small; a long-lived byte pipe to every database does not belong next to that key. The tunnel holds no GitHub credential at all. In the cluster it may only `get` and `list` CloudNativePG `Cluster`s, create Events in `argocd`, and `get` exactly the open roles' password Secrets, which each Environment grants it through a `Role` the application chart renders. Switching it off (`dbTunnel.enabled: false`) touches nothing else, the way Argus can be switched off.
- **The tunnel logs in on the developer's behalf.** It performs the Postgres startup and the SCRAM-SHA-256 exchange as the chosen role itself, answers the client's startup with `AuthenticationOk`, and then copies bytes both ways. No password leaves the cluster: nothing is handed out that would outlive the developer's access, needs rotating when someone leaves, or could be copied into a script on a laptop. Each WebSocket is one Postgres connection, and each runs the whole check again, so a permission taken away on GitHub takes effect at the next connection.
- **Limits and audit are the tunnel's.** A session ends after 30 minutes without traffic and after 8 hours in any case; both are constants. Each session's start and end, and each refusal, is a Kubernetes Event on the Environment's ArgoCD Application, which Argus shows, and a structured log line, which reaches Grafana Cloud. Neither holds SQL, query results, passwords or tokens.

## Considered options

- **A public Postgres port.** Open 5432 on the node, and route it to each Environment's database. Rejected: the firewall would have to open a port beyond 22, 80 and 443, every database would be reachable from the internet with nothing but its password in the way, and the passwords would have to be handed to developers, which is what the tunnel avoids.
- **Developer kubeconfigs and `kubectl port-forward`.** Rejected: it means giving developers access to the Kubernetes API, which nobody but the Platform admin has (ADR-0002, the firewall above), and RBAC on `pods/portforward` and Secrets that is far wider than one database role. The developer would still need the role's password.
- **A VPN** to the node, such as WireGuard or Tailscale. Rejected: a second login and a client to install for every developer, a new path into the cluster that is not GitHub's permissions, and the passwords still handed out.
- **A web SQL console** in the browser. Not chosen now, and tracked in #157: it needs an answer to who the person in the browser is, in GitHub terms, before it can follow repository permission. The tunnel answers that with the developer's own `gh auth` token.
- **Inside the Deploy gate's service.** One component fewer, and the gate already answers developers for `iidp app status`. Rejected for the reasons above: the gate stays small, and the component that pipes bytes to databases never holds the App key.

## Consequences

- The Platform has a new public endpoint, `db.<baseDomain>`, not behind Itema login. What protects it is the developer's GitHub token, checked with GitHub on every connection, and the levels on each Environment, which start closed on prod.
- Every connection costs two GitHub API calls (`GET /user` and `GET /repositories/<id>`) and a shallow clone of the Platform repository, made with the developer's own token and against their own rate limit. A Postgres client that opens many short connections is slower through the tunnel than direct.
- A developer who qualifies for read-write on an Environment can change its data, though not its schema, which stays with the migrations. The levels decide who; nobody else does.
- The Kubernetes Events last an hour, the API server's default; the log line in Grafana Cloud is the record that lasts, for as long as Grafana Cloud keeps logs (`docs/implementation-notes/159-database-tunnel.md`).
- The Platform admin's own access to the databases stays as it is: `kubectl` through the SSH tunnel (`infra/README.md`).
