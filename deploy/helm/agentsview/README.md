# AgentsView Helm chart

This chart runs AgentsView on Kubernetes as a shared server that reads from
PostgreSQL. People open the web UI in a browser, and MCP clients and scripts use
the same server through `agentsview mcp --server`. The chart runs one pod in one
of two modes:

| Mode       | What fills PostgreSQL                                                                  | Parser sandbox             | Data volume |
| ---------- | -------------------------------------------------------------------------------------- | -------------------------- | ----------- |
| `pg-serve` | Each workstation runs `agentsview replica pg push --watch`                             | Not used                   | Optional    |
| `hosted`   | Each workstation runs `agentsview source raw watch`; the server parses what it uploads | Needs a Kata runtime class | Required    |

Both modes run `agentsview replica pg serve`. The chart creates a Deployment, a
Service, a ConfigMap for `config.toml`, a PersistentVolumeClaim, a
ServiceAccount and, optionally, an Ingress and a provisioning Job. It creates no
namespace, no Secrets, no NetworkPolicy and no database.

Read [PostgreSQL Sync](../../../docs/pg-sync.md) for `pg-serve` and
[Hosted Raw Sync](../../../docs/hosted-raw-sync.md) for `hosted` before you
install. This README covers only what is specific to Kubernetes.

## Before you install

### Build an image

The chart has no default image, and rendering fails until you set
`image.repository` and `image.tag` (or `image.digest`). The upstream image is
published as `:latest` and lags releases, and hosted raw sync changes often on
`main`. Build the repository's `Dockerfile` from a commit you have chosen and
push it to a registry your cluster can pull from:

```bash
docker build \
  --build-arg VERSION=0.46.1 \
  --build-arg COMMIT="$(git rev-parse --short HEAD)" \
  -t registry.example.com/agentsview:0.46.1-"$(git rev-parse --short HEAD)" .
```

The hosted parser sandbox needs a Linux amd64 or arm64 build with cgo, which the
`Dockerfile` produces. `latest` is rejected as a tag.

### Prepare the namespace

The chart works under the Pod Security `restricted` profile. Every container
runs as uid 1000 with a read-only root filesystem, no privilege escalation, all
capabilities dropped and the `RuntimeDefault` seccomp profile.

The pod needs DNS and a route to PostgreSQL. If a network policy (Cilium or
otherwise) blocks egress by default, allow both for the release's pods. The
chart adds no policy of its own.

### Create the Secrets

The chart reads every credential from a Secret you create. It never takes a
password or token as a value.

| Secret                         | Keys                   | Used by                     | Required                       |
| ------------------------------ | ---------------------- | --------------------------- | ------------------------------ |
| `auth.existingSecret`          | `token`                | Web UI, API and MCP clients | When `auth.required` is true   |
| `cursorSecret.existingSecret`  | `cursor-secret`        | Signed pagination cursors   | In `hosted` mode               |
| `postgres.credentials`         | `username`, `password` | The running server          | Always                         |
| `hosted.provision.credentials` | `username`, `password` | The provisioning Job        | When the Job is enabled        |
| `postgres.caSecret`            | `ca.crt`               | TLS verification            | For `verify-ca`, `verify-full` |

```bash
kubectl -n agentsview create secret generic agentsview-auth \
  --from-literal=token="$(openssl rand -base64 32)"
kubectl -n agentsview create secret generic agentsview-cursor \
  --from-literal=cursor-secret="$(openssl rand -base64 32)"
```

### PostgreSQL from CloudNativePG

The defaults assume a CloudNativePG `Cluster` named `agentsview-db` in the
release namespace, managed outside this chart. CNPG creates the
`agentsview-db-rw` Service, the `agentsview-db-app` Secret (the application
owner, with `username` and `password` keys) and the `agentsview-db-ca` Secret.

The server's URL carries no credentials. The chart sets `PGUSER` and
`PGPASSWORD` from `postgres.credentials`, and the PostgreSQL driver reads them.
To verify the server certificate, set:

```yaml
postgres:
  sslMode: verify-full
  caSecret:
    name: agentsview-db-ca
```

Add every other role as a CNPG managed role with its own password Secret, then
point `postgres.credentials.existingSecret` at that Secret:

```yaml
# In the Cluster spec, outside this chart.
spec:
  managed:
    roles:
      - name: hosted_runtime
        ensure: present
        login: true
        superuser: false
        createdb: false
        createrole: false
        inherit: false
        replication: false
        bypassrls: false
        passwordSecret:
          name: agentsview-db-hosted-runtime
```

The password Secret has type `kubernetes.io/basic-auth` with `username` and
`password` keys.

## Mode: pg-serve

The server serves a schema that workstations fill with
`agentsview replica pg push --watch`. It parses nothing, so it needs no sandbox,
no runtime class and no persistent volume. The data directory then holds only
`config.toml` and the installation identity.

```yaml
image:
  repository: registry.example.com/agentsview
  tag: 0.46.1-1a2b3c4

postgres:
  schema: agentsview
  credentials:
    existingSecret: agentsview-db-reader

auth:
  existingSecret: agentsview-auth

server:
  publicURL: https://agents.example.com

persistence:
  enabled: false

ingress:
  enabled: true
  className: traefik
  hosts:
    - host: agents.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: agents-example-com-tls
      hosts: [agents.example.com]
```

A read-only role works. Legacy `pg serve` applies pending schema migrations at
startup when its role may, and otherwise needs a workstation's `pg push` to
upgrade the schema first. Each new table needs a `SELECT` grant for the reader;
see [Hosted Raw Sync](../../../docs/hosted-raw-sync.md#http-control-plane) for
the current list.

A schema that hosted raw sync owns refuses `pg push`. Use a separate schema for
workstation pushes.

## Mode: hosted

Devices upload original session files, and the server keeps them in a raw
custody repository under `/data/raw-sync/` on the persistent volume. With
`hosted.rawDerivation` (the default) it parses each upload in a sandbox and
writes sessions into one tenant's schema.

### The sandbox needs Kata

The parser runs each file in a jail built from user, mount and network
namespaces plus seccomp. Under runc on Ubuntu 24.04 nodes it fails closed:
containerd's default seccomp profile refuses `CLONE_NEWUSER` without
`CAP_SYS_ADMIN`, and Ubuntu's `kernel.apparmor_restrict_unprivileged_userns=1`
strips the jail's rights inside the namespace. No pod-spec change fixes that.

Kata runs the pod in a VM with its own kernel, where the jail works with
`RuntimeDefault` seccomp, uid 1000 and no capabilities. The sandbox tests pass
under `kata-clh-runtime-rs` and `kata-qemu-runtime-rs`. gVisor runs the jail but
does not enforce `RLIMIT_DATA` on `mmap`, so one resource limit fails.

Rendering fails when `hosted.rawDerivation` is true and `runtimeClassName` is
empty or lacks `kata`. Set `hosted.requireKataRuntime: false` only for a runtime
class you have tested with the sandbox suite (`RAW_SANDBOX_REQUIRED=1` on
`internal/rawderive`).

Kata changes resource planning:

- `kata-clh-runtime-rs` adds a fixed pod overhead of 250m CPU and 130Mi;
  `kata-qemu-runtime-rs` adds 320Mi.
- The memory limit sizes the VM. Parser children may use up to 1 GiB of data
  each next to the server's own working set, so start at a 3Gi to 4Gi limit.
- The data volume, `/tmp` and Secrets reach the guest through virtio-fs, which
  is slow for I/O-heavy work. Set `tmp.medium: Memory` to keep scratch space in
  guest RAM; it then counts against the memory limit.

### Set it up

1. Create the restricted runtime role as a CNPG managed role (see above). It
   must own no objects and have no role memberships.
1. Provision the tenant schema as the owner. Either enable the chart's Job,
   which runs `agentsview replica pg hosted-provision provision` before each
   install and upgrade, or run that command yourself.
1. Run the grants from
   [Hosted Raw Sync](../../../docs/hosted-raw-sync.md#provision-a-hosted-instance)
   as the owner, for example through `kubectl cnpg psql agentsview-db`. The
   image has no `psql`, so the Job cannot run them. Until the grants are
   complete, the server fails startup and Kubernetes restarts it.
1. Enroll each device and give it the server URL, device ID and credential.
   AgentsView has no enrollment command or endpoint yet.
1. On each machine, run `agentsview source raw backfill` once, then
   `agentsview source raw watch`.

```yaml
mode: hosted

image:
  repository: registry.example.com/agentsview
  tag: 0.46.1-1a2b3c4

runtimeClassName: kata-clh-runtime-rs

postgres:
  credentials:
    existingSecret: agentsview-db-hosted-runtime

hosted:
  schema: hosted_sessions
  tenant: tenant-example
  provision:
    enabled: true
    credentials:
      existingSecret: agentsview-db-app

auth:
  existingSecret: agentsview-auth

cursorSecret:
  existingSecret: agentsview-cursor

server:
  publicURL: https://agents.example.com

persistence:
  size: 50Gi

resources:
  requests:
    cpu: 500m
    memory: 1Gi
  limits:
    memory: 4Gi
```

Hosted mode requires `auth.required: true`, a cursor secret and a persistent
volume, and rendering fails without them. Back up the PostgreSQL database and
the volume together: PostgreSQL holds custody metadata, and the volume holds the
raw bytes it refers to.

## Authentication

With `auth.required: true` (the default), every `/api/` request needs the shared
bearer token from `auth.existingSecret`. The browser asks for it once and keeps
it in local storage. MCP clients pass it with
`agentsview mcp --server https://agents.example.com --server-token-file <path>`.

Raw-sync routes (`/api/v1/raw-sync/*`) ignore the shared token. Devices
authenticate with their own credentials and short-lived scoped tokens.

AgentsView has no OIDC or per-user sign-in yet. Until it does, a forward-auth
proxy such as oauth2-proxy or Authelia can sign people in before they reach the
UI. Exclude two kinds of traffic from it:

- `/api/v1/raw-sync/*`, because devices cannot sign in interactively.
- Requests carrying `Authorization: Bearer`, because MCP clients and scripts
  cannot either. The server checks that token itself.

A Traefik example, with the oauth2-proxy middleware defined elsewhere:

```yaml
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: agentsview
spec:
  entryPoints: [websecure]
  routes:
    - match: Host(`agents.example.com`) && PathPrefix(`/api/v1/raw-sync/`)
      priority: 30
      services:
        - name: agentsview
          port: 8080
    - match: Host(`agents.example.com`) && HeaderRegexp(`Authorization`, `^Bearer .+`)
      priority: 20
      services:
        - name: agentsview
          port: 8080
    - match: Host(`agents.example.com`)
      priority: 10
      middlewares:
        - name: oauth2-proxy
      services:
        - name: agentsview
          port: 8080
  tls:
    secretName: agents-example-com-tls
```

Keep `auth.required: true` with this layout. The bearer route skips the proxy,
so with authentication off anyone could send any `Authorization` header and
reach the API.

In `pg-serve` mode you may turn authentication off and let the proxy guard
everything. Then:

- Set `server.publicURL` to the proxy's browser URL (rendering fails without
  it), and list any other browser origins in `server.publicOrigins`. The server
  checks `Host` and `Origin` against them.
- Drop the bearer route above. MCP clients and scripts must reach the server
  some other way that is not public, such as `kubectl port-forward`, because the
  server trusts every request it receives.
- Make sure nothing but the proxy can reach the pod.

The chart passes `--host=0.0.0.0` as a flag, which `pg serve` accepts without
authentication. `AGENTSVIEW_HOST` would refuse it.

`replica pg serve` ignores `public_url` and `public_origins` in `config.toml`,
so the chart passes them as `--public-url` and `--public-origin`.

## Health checks

The startup, liveness and readiness probes request `<basePath>/`, the web UI's
root. Static assets need no token and skip `Host` checks. Every `/api/` route,
`/api/ping` included, returns 401 without the token, so the probes cannot use
them. The probes show that the listener is up; they do not show that PostgreSQL
is reachable or that the parser worker is healthy.

## Storage, configuration and logs

`config.toml` must live in the data directory (`/data`), and the server writes
other files next to it. An init container copies the ConfigMap's `config.toml`
there on every start and prepends `cursor_secret` from its Secret. Changes made
in the web UI's settings do not survive a restart; change the chart's values
instead. `config.extraSettings` adds top-level keys and `config.extraTables`
adds whole tables.

The server writes its log to `/data/debug.log`. With `server.logToStderr: true`
(the default) the init container makes that file a symlink to the container's
stderr, so `kubectl logs` shows it. The server never rotates a symlinked log.

`/tmp` is an `emptyDir`. `HOME` and `TMPDIR` point at it, so nothing writes to
the read-only root filesystem.

The Deployment runs one replica with the `Recreate` strategy. The data volume is
`ReadWriteOnce`, and each hosted tenant must have exactly one server.

## Limits

- The raw custody repository keeps a SQLite catalog under `/data/raw-sync/`. Its
  locking and fsync behavior on a volume reached through virtio-fs under Kata is
  untested. Check it before the volume holds data you cannot rebuild.
- The parser sandbox has passed its tests under Kata without a read-only root
  filesystem. This chart sets `readOnlyRootFilesystem: true`; if the server
  reports isolation as unavailable, try
  `securityContext.readOnlyRootFilesystem: false` before suspecting the runtime
  class.
- When the sandbox fails, the child exits with code 78 and nothing on stderr, so
  the server reports only that isolation is unavailable.
- Semantic search does not work on hosted sessions, and hosted sessions carry no
  archive labels.
- Device enrollment has no command or endpoint.

## Values

The most-used values follow. `values.yaml` documents every value, and
`values.schema.json` validates them.

| Value                           | Default                  | Purpose                                                      |
| ------------------------------- | ------------------------ | ------------------------------------------------------------ |
| `mode`                          | `pg-serve`               | `pg-serve` or `hosted`                                       |
| `image.repository`, `image.tag` | none                     | Required; pin an exact build                                 |
| `runtimeClassName`              | empty                    | Pod runtime class; a Kata class for hosted raw derivation    |
| `postgres.host`                 | `agentsview-db-rw`       | PostgreSQL host                                              |
| `postgres.database`             | `app`                    | Database name                                                |
| `postgres.sslMode`              | `require`                | `require`, `verify-ca` or `verify-full`                      |
| `postgres.schema`               | `agentsview`             | Schema served in `pg-serve` mode                             |
| `postgres.credentials`          | `agentsview-db-app`      | Secret with the server's login                               |
| `hosted.tenant`                 | empty                    | Required in `hosted` mode                                    |
| `hosted.schema`                 | `hosted_sessions`        | Tenant schema                                                |
| `hosted.rawDerivation`          | `true`                   | Parse uploads into PostgreSQL                                |
| `hosted.provision.enabled`      | `false`                  | Run `hosted-provision` as a pre-install and pre-upgrade hook |
| `auth.required`                 | `true`                   | Require the bearer token on `/api/`                          |
| `auth.existingSecret`           | empty                    | Secret with the bearer token                                 |
| `cursorSecret.existingSecret`   | empty                    | Secret with the cursor HMAC key                              |
| `server.publicURL`              | empty                    | Browser URL behind an ingress or proxy                       |
| `server.basePath`               | empty                    | URL prefix when mounted below a path                         |
| `persistence.enabled`           | `true`                   | Persistent data volume; required in `hosted` mode            |
| `persistence.keepOnUninstall`   | `true`                   | Keep the PVC when the release is uninstalled                 |
| `resources`                     | 256Mi request, 1Gi limit | Raise the limit to 3Gi or more for hosted raw derivation     |
