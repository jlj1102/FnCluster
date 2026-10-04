# FnCluster

Go agent framework for independent fnOS nodes. The current build supports one-time node join, mTLS health exchange, stable node identity, Linux metrics, aggregate capacity statistics, FPK application discovery, a read-only management page and recoverable WebUI loader patching. Remote fnOS native panels are still experimental.

## Run

```bash
go build -o fncluster ./cmd/fncluster
./fncluster serve --data-dir ./state --storage-mount /vol1
```

Open `http://127.0.0.1:38670/` on the host or through an SSH tunnel. By default the Linux collector discovers fnOS `/volN` mounts and deduplicates them by filesystem device. Repeat `--storage-mount` to override discovery with explicit mounts. **Cluster Storage** is only a sum of independent filesystems.

The TCP HTTP API only accepts loopback listeners until fnOS user authorization and reverse proxy integration are verified. Its read routes include `GET /healthz`, `/api/v1/cluster`, `/api/v1/nodes`, `/api/v1/nodes/{id}`, `/api/v1/storage/summary`, and `/api/v1/panels`. There is no shell endpoint. The FPK provides the same API over a local Unix socket through the fnOS application gateway, where the browser management page can also create/join a cluster and manage WebUI integration.

## Join nodes

Use the FnCluster desktop page on each node: create a cluster, generate a one-time pairing code (`token.fingerprint`), then paste it with the seed address on the other node. The mesh listener starts automatically within seconds; no service restart is needed. The separate node transport uses port `38671` and never reuses fnOS WebSocket credentials. The same CLI works (`cluster init|token|join|remove|leave|status`). The cluster creator can revoke a member; members can leave. See [docs/CLUSTER_JOIN.md](docs/CLUSTER_JOIN.md) for steps and security limits.

The WebUI reverse-engineering findings for fnOS 1.2.0701 are in [docs/FNOS_WEBUI_1.2.0701_FINDINGS.md](docs/FNOS_WEBUI_1.2.0701_FINDINGS.md). The CDIfn/Fndesk comparison and remaining validation steps are in [docs/FNOS_FPK_WEBUI_REFERENCES.md](docs/FNOS_FPK_WEBUI_REFERENCES.md). Remote Settings remains experimental because the official frontend binds it to the desktop's main RPC singleton.

## fnOS package

On Windows, `./packaging/fnos/build.ps1 -Arch amd64` produces [x86 FPK](packaging/fnos/fncluster.fpk); `-Arch arm64` produces [ARM FPK](packaging/fnos/fncluster-arm.fpk). The package registers a fnOS desktop icon that opens the standalone FnCluster page inside an iframe. Its `/app/fncluster/` route uses the Unix socket gateway shape found in Fndesk. Confirm route access control on the target fnOS before adding mutating browser APIs. See [packaging/fnos/README.md](packaging/fnos/README.md).

## WebUI integration

The loader is served at `/app/fncluster/inject/loader.js` through the FPK gateway. It renders a cluster node badge and aggregate storage text on the verified 1.2.0701 asset build. Patch application is an explicit action from the desktop page or the CLI. It checks the exact original `index.html` hash, saves a backup, applies one marked script, and checks the served page. Because fnOS's `trim_file_monitor` restores `/usr/trim/www` from `/usr/trim/share/.restore/www.zip`, the same entry is kept in sync inside that archive; the shipped archive is encrypted, so when its entries cannot be read the archive is rebuilt as plaintext from the live www tree (fnOS accepts plaintext restore archives). The pristine archive is copied to `/usr/trim/share/.restore/www-before-cluster.bak` before the first change, and a working copy is kept in the state directory and restored on recovery. If the gateway returns fnOS's unauthenticated `invalid token` response for the loader, it verifies the loader through the local FPK socket; the authenticated desktop still needs a browser check. Transient desktop restarts are retried (6 × 5 s) before rollback, and an unregistered build can be patched only with an explicit compatibility confirmation after structure validation. A failed check restores the original. The browser page shows patch/archive state and tails the service and audit logs; every browser action is written to the local audit log. The loader also bridges the native desktop widget (0.1.11): a node switch next to its display settings shows the local node natively, feeds remote-node metrics into the native cards, or renders a per-node overview; it is browser-only and disables itself on unrecognized builds.

```bash
./fncluster webui status
./fncluster webui apply \
  --loader-file /var/apps/fncluster/target/inject/loader.js \
  --health-url http://127.0.0.1:5666/
./fncluster recover webui
./fncluster webui enable  # only after checking the supported WebUI build
```

The FPK contains that loader path. The `--health-url` must point to the local fnOS WebUI. Recovery works without the agent or browser. See [docs/WEBUI_RECOVERY.md](docs/WEBUI_RECOVERY.md).

The shipped management page and loader live in `internal/api/web` and are embedded in the Go binary; `web/panel/` contains research-only browser scripts and is not packaged. Remote native panel design and its open question are in [docs/PANEL_ACCESS.md](docs/PANEL_ACCESS.md); the two-node acceptance runbook is [docs/DEVICE_VALIDATION.md](docs/DEVICE_VALIDATION.md).

## Current limits

- Node join and mTLS state exchange pass a local two-node integration test, but have not yet been verified on two fnOS machines. Member removal is implemented as controller-only revocation (`cluster remove`); real two-node propagation is still unverified.
- Host metrics require Linux `/proc`; running the HTTP process on Windows is useful for UI/API development only.
- The x86 FPK was upgraded on fnOSTest (fnOS 1.2.0701) and its administrator desktop page showed numeric memory, `/vol1` storage, and discovered panels. ARM installation, two-node operation, and the browser management flow still need target-system testing.
- Browser management has no separate management password: it trusts the fnOS gateway login as the identity boundary and requires the `X-FnCluster: 1` header plus same-origin access. Device checks confirmed unauthenticated GET/POST are blocked and that a root-owned `0600` socket keeps the desktop working, so the socket no longer accepts other local users. A revoked node can be re-admitted entirely from the browser (new pairing code + “重置节点身份”).
- Native remote Settings/Files are not connected: the captured fnOS build binds Settings to the desktop's main RPC singleton, and remote fnOS user authentication remains separate from cluster membership.
- Storage numbers are sums of configured local mounts, never a virtual filesystem or pool.
