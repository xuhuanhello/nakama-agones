# Private access and credentials

The default listener is VPS loopback `127.0.0.1:7365`. No DNS name, public ingress, public TLS route or cloud firewall opening is needed. Keep SSH host-key verification enabled. Public game API HTTPS and private cluster communication remain available.

## 1. Establish a tunnel

Replace `NAKAMA_HOST` with the Nakama host, not the regional K3s manager. Enable the Nakama Console forward only if its 7351 listener is already bound to VPS loopback.

### macOS / Linux (POSIX shell)

```sh
ssh -N -o ExitOnForwardFailure=yes \
  -o ServerAliveInterval=30 -o ServerAliveCountMax=3 \
  -L 127.0.0.1:17365:127.0.0.1:7365 \
  -L 127.0.0.1:17351:127.0.0.1:7351 \
  root@NAKAMA_HOST
```

### Windows PowerShell (OpenSSH)

```powershell
ssh -N -o ExitOnForwardFailure=yes `
  -o ServerAliveInterval=30 -o ServerAliveCountMax=3 `
  -L 127.0.0.1:17365:127.0.0.1:7365 `
  -L 127.0.0.1:17351:127.0.0.1:7351 `
  root@NAKAMA_HOST
```

The terminal stays quiet after authentication; leave it running. Open:

| Page | Local URL | Login |
| --- | --- | --- |
| Fleet console | `http://127.0.0.1:17365/fleet-admin/` | Independent Fleet administrator |
| Nakama Console | `http://127.0.0.1:17351/` | Existing Nakama Console account |

Use `127.0.0.1`, not `localhost`, unless the configured `public_url` is changed. The console checks the exact Host and Origin. Do not substitute port 7350: that is Nakama's HTTP API, not a dashboard.

A separate load generator may also need `-L 127.0.0.1:17350:127.0.0.1:7350`. Its private admin token is for the trusted generator process, never a browser. This third forward is not needed for viewing Fleet Console.

If a port is occupied, reuse the existing verified tunnel or stop that specific old forward. On macOS/Linux inspect `lsof -nP -iTCP:17365 -sTCP:LISTEN`; on PowerShell use `Get-NetTCPConnection -LocalPort 17365 -State Listen`. Do not disable host-key checking to resolve a key mismatch: verify the host fingerprint through a trusted channel first.

## 2. Where the password actually lives

| Material | Authoritative location | What a local credentials file means |
| --- | --- | --- |
| Fleet username + password hash | `/etc/fleet-console/config.json` on the Nakama VPS | Optional operator copy of the originally generated password; not read by the server |
| Nakama Console account | Existing private Nakama configuration/environment | Separate account, unchanged by Fleet console installation |
| Read-only API token hash | `read_api_token_hash` in Fleet configuration | Original token must be retained in an operator-only secret file |
| Kubernetes observer token | Rotating private token file on the VPS | Not an administrator login and never given to browsers |
| Original Fleet administrator token | Existing root-only runtime credential file | The optional action broker reads it; the web process does not receive it |

`fleet-console.service` reads its configuration at startup. The browser sends the entered password to this service through the encrypted SSH tunnel; it verifies the hash. A local Git-ignored password copy is not a deployment dependency. Deleting that copy does not stop the service, but the hash cannot reveal a forgotten password.

To check the Fleet username on the VPS without printing its hash or other configuration:

```sh
sudo python3 -c 'import json; print(json.load(open("/etc/fleet-console/config.json"))["username"])'
```

## 3. Reset a forgotten Fleet password

SSH into the VPS as the authorized operator. Use hidden input; do not put passwords in shell arguments, environment variables, examples or Git:

```sh
python3 - <<'PYRESET'
import getpass, subprocess
password = getpass.getpass('New Fleet password: ')
if password != getpass.getpass('Confirm password: '):
    raise SystemExit('Passwords differ; no change.')
subprocess.run(['/usr/local/bin/fleet-console', 'set-password', '--config',
                '/etc/fleet-console/config.json'],
               input=(password+'\n').encode(), check=True)
PYRESET
systemctl restart fleet-console
```

The CLI updates the owner-only file atomically and preserves ownership. Restarting **only** Fleet console invalidates its existing browser sessions and applies the new hash. It does not restart Nakama, game processes, databases or the cluster. Update the operator's private password copy/password manager separately.

## 4. Script/AI access

Use the [versioned read API](console-api.md) and an independently generated read-only token. It works only on the fixed GET routes, never login/session or management mutations. The original token is shown once by `fleet-console generate-api-token`; redirect it into a private file and install only its hash in the server configuration. Do not put a token in a URL or a public issue. The API documentation gives invocation and pagination examples.

For temporary manual operations, browser sessions and CSRF remain supported. The optional Unix action broker allows only instance drain and creation retry. It is not a Kubernetes shell or arbitrary proxy.

## 5. Failure checklist

| Symptom | Check |
| --- | --- |
| Connection refused | Local tunnel, VPS service status and loopback port |
| `invalid_host` / 421 | Exact `public_url`, local hostname and port |
| Login 401 | Fleet credentials, not the Nakama Console password |
| Login 429 | Wait for the bounded login-attempt window; do not loop retries |
| Session expired | Sign in again; changing/restarting console invalidates sessions |
| Fleet snapshot stale | `fleet-console-export.timer` and exporter service; do not fabricate zero occupancy |
| Upstream access denied | Independent observer token issue/sync and scoped RBAC |
| Management unavailable | Explicit `allow_management`, broker socket/service and runtime status |

Do not print entire environment files, service account tokens, Nakama startup logs or full Docker inspections while troubleshooting.
