# Synchronize an external Nakama token

`scripts/sync_nakama_token.py` consumes the schema-1 JSON from [the token issuer](credentials.md). A dedicated SSH key can read only the issuer's private bundle. Every five minutes the receiver validates its API URL, namespace, ServiceAccount, JWT subject, expiry and remaining lifetime, then replaces the token in the mounted credentials directory. The default acceptance window is 15 minutes to two hours. The issuer normally requests one hour and refreshes every 15 minutes; a 20-minute issuer interval also fits this window.

SSH host-key verification authenticates the issuer. The receiver checks JWT claims but does not locally verify the signature; Kubernetes verifies it when Nakama uses the token. No administrator kubeconfig, K3s join token, token value or private key belongs in the repository, unit arguments or logs.

## Prepare the restricted connection

On the Nakama host, create root-owned `0700` directories `/opt/nakama-agones/credential-sync` and `/opt/nakama-agones/credentials`. Generate a dedicated, unattended SSH key in `credential-sync/token-pull.key` with mode `0600`; it must not be a personal administrator key. Put the manager's **independently verified** SSH host key in `credential-sync/known_hosts` (root-owned `0600` or `0644`). A key collected by `ssh-keyscan` alone is not verification.

On the manager, authorize only its public key, restricted to Nakama's source address and the fixed export file. Replace the uppercase placeholders:

```text
from="NAKAMA_SOURCE_CIDR",restrict,command="/usr/bin/cat /var/lib/nakama-agones/token-export/nakama.json" ssh-ed25519 PUBLIC_KEY
```

The issuer writes a root-owned `0600` bundle inside a `0700` directory, so this example uses a restricted root key. It allows no shell, PTY, forwarding or choice of file. The receiver always sends the literal request `nakama-agones-token-bundle-v1`; there is no option to pass a remote shell command. The forced command must ignore that request and read only the fixed file. Do not grant an unrestricted root key for this workflow.

## Install on the Nakama host

Copy the script to `/opt/nakama-agones/scripts/sync_nakama_token.py`. It needs only Python 3.9+, OpenSSH and standard Linux libraries. Copy `deploy/systemd/nakama-agones-token-sync.env.example` to `/opt/nakama-agones/credential-sync/config.env` with root ownership and mode `0600`, then fill in the **non-secret** host names, paths and expected identity. The API URL must match the issuer bundle and use HTTPS; credentials, path suffixes, query parameters and fragments are rejected.

Copy the `.service` and `.timer` files from `deploy/systemd/` to `/etc/systemd/system/`, then run:

```sh
systemctl daemon-reload
systemctl start nakama-agones-token-sync.service
python3 /opt/nakama-agones/scripts/sync_nakama_token.py --status
systemctl enable --now nakama-agones-token-sync.timer
```

A successful `--status` returns `ok: true` and a `credential` object containing `state`, `expires_at`, `installed_at` and `remaining_seconds`. Check for `credential.state=valid` before starting the provider. If your deployment uses a different credentials path, change **both** the service's `--output-dir` and `ReadWritePaths`, and pass the same `--output-dir` to `--status`. The script requires the directory to exist with mode `0700`; it does not change directory ownership or the CA file.

Mount the entire credentials directory read-only at `/run/agones-kubernetes`. Configure `AGONES_KUBERNETES_TOKEN_FILE=/run/agones-kubernetes/token` and provision the verified CA at `/run/agones-kubernetes/ca.crt`. The container process must be able to read the root-owned `0600` token. The Go provider reads the token on each API request, so rotation does not require a Nakama restart. A bind mount of the individual token file would retain the previous inode and must not be used.

## Failure and verification

SSH is noninteractive, uses only the selected key and known-hosts file, ignores ambient SSH configuration/agents, rejects unknown host keys, and has a 30-second total deadline with a 64 KiB response limit. Remote stdout is JSON data only; remote stderr and exception text are never printed. Failed transport, validation or writes before token replacement leave the previous token in place and return exit code 1. There is no fallback identity or restart/delete operation.

`token` and `token.metadata.json` are written as `0600` files using same-directory temporary files, fsync and atomic renames. Metadata records the expiry and a checksum of the installed file, never the token itself. The token rename is the commit point. A caught token-rename failure restores the previous metadata; interruption between the two renames is detectable as `metadata_mismatch` instead of incorrectly claiming a new expiry. The next successful sync repairs it. A storage failure after the commit point reports `commit_durability_unknown`; inspect status and the host storage before assuming either version is durable.

Monitor the service result, delivery age (`installed_at`) and remaining lifetime. Alert on any failed sync and before less than 15 minutes remain. `--status` is entirely local and returns nonzero for missing, expired or inconsistent files; it does not prove Kubernetes has not revoked the credential. After the first installation and a later rotation, make a normal scoped provider request to confirm API access without restarting Nakama. Tokens and credential directories must stay out of logs, backups accessible to clients and source control.

Offline regression tests require no cluster or SSH endpoint:

```sh
python3 -m unittest discover -s tests -p 'test_sync_nakama_token.py'
```
