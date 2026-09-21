# Install the Fleet console

The console is an optional component of **this plugin repository and its releases**. `agones.so` runs inside Nakama; `fleet-console` runs as a separate loopback web/API service. This keeps UI and log tooling upgrades independent of live matchmaking. It requires no player database and no Nakama SDK changes.

Read [access and passwords](console-access.md), [daily operation](console-operations.md) and the [API reference](console-api.md) after installation. Historical log setup is a separate [Loki/Alloy step](console-logs.md).

## 1. Obtain the package

Use a verified release archive containing `fleet-console` / `fleet-console-control`, or build the console-only bundle with the pinned Go toolchain:

```sh
./scripts/package-console.sh
```

It produces a Linux amd64 archive in `dist/`, including binaries, public configuration/systemd templates, helpers, docs and SHA256SUMS. It does not rebuild or restart Nakama. From source, individual binaries can also be built with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -mod=readonly` and their `./cmd/...` paths.

## 2. Prepare the service account and observer

On the Nakama host, create an unprivileged `fleet-console` system user/group. Install the binaries in `/usr/local/bin/`. Create `/etc/fleet-console` as `root:fleet-console` mode `0750` and the private service configuration as owner `fleet-console`, mode `0600`.

On each observed cluster, apply [observer-rbac.yaml](../deploy/console/observer-rbac.yaml). The dedicated `agones-control/fleet-console` identity reads nodes, scoped Pod/event/metric data and logs. It cannot read Secrets, execute containers, delete Pods or administer the cluster.

Install the trusted cluster CA and a **short-lived, separately rotated** observer token on the console host. Follow [token issuance](credentials.md) and [token delivery](token-sync.md), using separate export paths, SSH receiver key, ServiceAccount and timer/service names. Do not reuse or replace Nakama's runtime identity. The console reads its token file on every request, so normal rotation needs no restart.

## 3. Configure the web service and password

Copy [config.example.json](../deploy/console/config.example.json) to `/etc/fleet-console/config.json`. Set `public_url` to the administrator's **SSH-local browser URL**, normally `http://127.0.0.1:17365/fleet-admin/`; it does not mean the page is public. Set `listen` to VPS loopback `127.0.0.1:7365`.

Configure region names, private Kubernetes API URLs and CA/token paths. Region names must match Alloy's `FLEET_LOG_CLUSTER`. `max_processes` / `rooms_per_process` are display settings and must match the actual runtime; editing them does not change capacity. Multiple observed regions do not automatically enable cross-region matchmaking.

Set the initial password without writing it in shell history, then retain the password in an operator-owned password manager. This example reads it invisibly and stores only its hash:

```sh
python3 - <<'PYINIT'
import getpass, json, subprocess
from pathlib import Path
path = Path('/etc/fleet-console/config.json')
config = json.loads(path.read_text())
password = getpass.getpass('Initial Fleet password: ')
if password != getpass.getpass('Confirm password: '):
    raise SystemExit('Passwords differ; no change.')
config['password_hash'] = subprocess.check_output(
    ['/usr/local/bin/fleet-console', 'hash-password'],
    input=(password+'\n').encode()).decode().strip()
path.write_text(json.dumps(config, indent=2)+'\n')
path.chmod(0o600)
PYINIT
```

A local Git-ignored password copy is optional and is never read by the deployed service. If it is lost, reset the password on the VPS as described in [credential recovery](console-access.md#3-reset-a-forgotten-fleet-password).

## 4. Publish a credential-free Fleet snapshot

Create `/var/lib/fleet-console/status` as `root:fleet-console` mode `0750`. Install `scripts/export_console_status.py` under `/opt/nakama-agones/scripts/`. Install the [export service](../deploy/console/fleet-console-export.service) and [timer](../deploy/console/fleet-console-export.timer), adjusting only deployment-specific paths and the private Fleet loopback URL.

The root exporter reads the **existing** runtime credential file, calls only `GET /agones/fleet/v1/admin/status`, and atomically publishes a whitelist snapshot every five seconds. The web service never receives that credential. Failed exports retain the previous file; a snapshot over 20 seconds old is explicitly unavailable.

Enable the exporter and [web service](../deploy/console/fleet-console.service):

```sh
systemctl daemon-reload
systemctl enable --now fleet-console-export.timer
systemctl start fleet-console-export.service
systemctl enable --now fleet-console
```

The web service runs without Linux capabilities, with a read-only filesystem, a 256 MiB memory ceiling and a 0.5 CPU quota. It is read-only unless the optional next step is configured.

## 5. Optional restricted management

Install the [control broker service](../deploy/console/fleet-console-control.service) and copy its private config example to `/etc/fleet-console/control.json`, owned by root with mode `0600`. It references the original root-only runtime credential file; do not copy the full Fleet token into the web account.

The broker exposes only fixed instance-drain and creation-retry requests over `/run/fleet-console-control/control.sock`. The socket is `root:fleet-console` mode `0660`, inside a `0750` directory. There is no shell, arbitrary upstream URL, Kubernetes mutation proxy or public port.

Set the web configuration's `source.control_socket` to that path and `source.allow_management` to `true`, then enable the broker and restart **only** the console. Management appears only when the snapshot and broker are usable. A failed broker does not erase room visibility; it reports a separate management error.

Read-only API credentials are a separate option in [API setup](console-api.md); they cannot invoke these actions. Direct Fleet-token mode remains an explicit advanced configuration, never a fallback from a missing snapshot or failed broker.

## 6. Logs, SSH access and acceptance

Enable the [optional log collector/store](console-logs.md), establish a [Shell or PowerShell SSH tunnel](console-access.md), and log in. Do not add a public proxy route or firewall opening for 7350/7351/7365.

Verify a real room and its player seats, metrics, live/history logs, then test a graceful drain on a test instance. Check filter/form/scroll stability across automatic updates. Verify observer permission denials, read-token mutation denial, token rotation and that public management routes remain unavailable. A mock alone is not deployment evidence.

For upgrades, preserve private configuration and local log PVCs, install verified new binaries, then restart only the changed web/broker service. Browser sessions are in memory and must sign in again after a web-service restart. Replacing `agones.so` is a separate planned Nakama deployment.
