# Deploy Nakama and GameFleet service from a blank Linux host

This is the single supported Nakama deployment path: a full Compose stack with PostgreSQL, the complete application Nakama image, an HTTPS gateway, and a private SSH forward to GameFleet's loopback Business listener. The commands prepare and run the Nakama host. They do not install or modify the separate GameFleet platform.

The helper never receives a password argument. It generates local database and Nakama keys without printing them, validates file permissions from metadata, checks a local image and module files, and runs Compose's quiet model validation. It does not SSH, publish an image, start containers, or claim remote health.

## Requirements and owner-provided inputs

Use a Linux amd64 host with Docker Engine, the Docker Compose plugin, and Python 3.10 or later. Confirm the host has free TCP ports 80/443 and unused Docker subnets for the two bridges. The Nakama plugin ABI in this repository targets `linux/amd64` and the pinned toolchain in [compatibility](compatibility.md).

Before starting, obtain these inputs through the responsible owner systems:

- A GameFleet service identity, its private `gfsvc_` key, exact application/issuer/region/compatibility values, search and match grants, and History routes.
- A GameFleet platform SSH account and dedicated public key authorization restricted to forwarding only to `127.0.0.1:17682`. Get the server host key fingerprint through a trusted channel and install that verified key in `known_hosts`.
- A complete Nakama application image from Fixed's `deploy/account/build.py`. Build it on this Nakama host when using a local image ID, or publish it through your own registry and use the immutable repository digest. The image must contain `/nakama/data/modules/agones.so`; for Fixed account login, it must also contain `/nakama/data/modules/account.so`.
- A DNS name pointing at the Nakama host. Allow inbound public TCP 80/443 (and UDP 443 only if you want HTTP/3); restrict SSH administration and outbound SSH egress according to your host policy.

The remote platform must already bind its Business listener to its own `127.0.0.1:17682`. This stack does not switch the platform to host networking, publish that management listener, or route it through the public HTTPS gateway.

## Create the host project

Obtain the source and pin the exact revision on the Nakama VPS. The working branch below contains this deployment entry; record the printed commit in your deployment record. For a selected release, checkout its reviewed commit instead.

```sh
sudo git clone --branch codex/self-service-bootstrap https://github.com/xuhuanhello/nakama-agones.git /opt/nakama-source
cd /opt/nakama-source
git rev-parse HEAD
sudo git checkout --detach "$(git rev-parse HEAD)"
```

Run initialization, validation, and Compose with host administrator privileges. The stack directory and tunnel credential files belong to root; the SSH sidecar retains `cap_drop: ALL` and must be able to read its owner-only files without bypassing permissions:

```sh
sudo install -d -m 0700 -o root -g root /opt/nakama
sudo python3 /opt/nakama-source/scripts/nakama_stack.py init --directory /opt/nakama
```

`init` copies the complete Compose project to `/opt/nakama`, generates random database and Nakama keys under `private/` with mode `0400`, and creates empty owner-supplied credential placeholders. It refuses to overwrite any existing file. The generated files are local inputs; the stack is not running yet.

The socket server key is shared with clients as Nakama's application key. Fixed currently reads it from the `OnlineClient.gameFleetNakamaServerKey` field in the `Game_Online` scene; its checked-in default is `local-server-key`. Read the generated `private/nakama-socket-server-key` locally and synchronize its value with that Unity component before building a client. Do not place the database password, session keys, runtime HTTP key, or console credentials in the client.

## Configure the image, DNS, networks, and service

Edit `/opt/nakama/.env` using `sudoedit`. The template pins public dependencies to tested repository digests; keep those defaults for the first deployment. If deliberately updating a dependency, pull its release family and record the returned repository digest:

```sh
sudo docker pull --platform linux/amd64 postgres:16-alpine
sudo docker image inspect --format '{{index .RepoDigests 0}}' postgres:16-alpine
sudo docker pull --platform linux/amd64 caddy:2-alpine
sudo docker image inspect --format '{{index .RepoDigests 0}}' caddy:2-alpine
sudo docker pull --platform linux/amd64 alpine:3.21
sudo docker image inspect --format '{{index .RepoDigests 0}}' alpine:3.21
```

For an intentional dependency update, copy these complete outputs into `POSTGRES_IMAGE`, `CADDY_IMAGE`, and `ALPINE_IMAGE`, respectively. Deployment consumes the recorded digest, not a mutable tag. Verify a changed digest in your deployment environment before promotion. Do not copy an image config ID into these dependency fields.

Set `NAKAMA_RUNTIME_IMAGE` to either:

- `sha256:<64-hex-local-image-id>` returned by the Fixed helper when the full image was built on this host; or
- `registry.example/repository@sha256:<64-hex-digest>` after pulling that exact image onto this host.

Set `APPLICATION_REQUIRED_MODULES=account.so` for Fixed account login. Leave it empty only if the application genuinely has no extra required module. `nakama_stack.py validate` checks each filename against the image without network access, and Compose uses `pull_policy: never` for the application so it cannot silently replace a local image. For a registry image, pull the exact digest explicitly before validation.

Set `NAKAMA_DOMAIN` to the public DNS name and `GAMEFLEET_SSH_TARGET` to the dedicated `user@host` for the platform tunnel. Keep the default Docker networks only if they do not overlap any existing networks or routed VPN ranges; otherwise choose two non-overlapping IPv4 subnets and distinct static frontend addresses. The default Caddy address is `172.29.240.2` and Nakama is `172.29.240.3`.

Copy the owner-issued service key to `/opt/nakama/private/gamefleet-service-key`. Set the seven exact service fields in `/opt/nakama/private/gamefleet-service.env`; its in-container key path must remain `/run/secrets/gamefleet-service-key`. The service URL is exactly `http://127.0.0.1:17682` inside the shared Nakama/tunnel network namespace. Set every identity and profile field to the value provisioned by the GameFleet owner; do not infer values from the image tag.

Install a dedicated SSH private key at the path configured by `PLATFORM_SSH_KEY_SOURCE` (default `./private/platform-ssh-key`) and a verified host key at `PLATFORM_KNOWN_HOSTS_SOURCE` (default `./private/platform-known-hosts`). The client runs with `BatchMode=yes` and `StrictHostKeyChecking=yes`; it will not ask for a password or accept a new host key automatically. Install both tunnel files as root-owned regular files with mode `0400`; local validation rejects a different owner. Use `sudo install -o root -g root -m 0400` to copy your verified files into the configured paths. The service key and application credentials are also private files. Do not use an unverified `ssh-keyscan` result as the only trust step.

On the platform host, restrict the tunnel user's SSH configuration to local forwarding and this one destination. A representative `sshd_config` Match block is:

```text
Match User nakama-tunnel
    AllowTcpForwarding local
    PermitOpen 127.0.0.1:17682
    PermitTTY no
    AllowAgentForwarding no
    X11Forwarding no
    GatewayPorts no
```

The authorized key should also use a forwarding-only restriction, for example `restrict,port-forwarding,permitopen="127.0.0.1:17682"` before the public key. Verify the effective SSH policy on that host and permit inbound SSH only from the Nakama host or its private network. The Nakama sidecar opens only a local `127.0.0.1:17682` listener and reconnects if the SSH process exits.

For Fixed account login, copy the Fixed-owned `account.env` to `/opt/nakama/private/account.env` and set `DM_ACCOUNT_TRUSTED_PROXY_CIDRS` to the exact Caddy address with `/32` (default `172.29.240.2/32`). The Caddyfile removes any client-supplied `X-DM-Client-IP` and replaces it with the direct TLS peer address. Do not trust the whole Docker subnet. If the Caddy IP changes, change the Fixed setting at the same time. The application env file is optional; this generic stack does not define SES or other provider-specific environment fields.

If the Fixed account module uses a credentials file, set `APPLICATION_SECRET_FILE` to its private source path and mount target `APPLICATION_SECRET_TARGET=account_credentials`. Fixed's env file should refer to `/run/secrets/account_credentials`. Keep the file read-only with mode `0400` or `0600`. The generated empty JSON placeholder is only for applications that do not consume an application credential file.

Generated database and Nakama secrets remain in private files. The PostgreSQL container reads its password through `POSTGRES_PASSWORD_FILE`. The Nakama entrypoint validates the safe generated key format and renders its YAML to `/run/nakama-config`, a container tmpfs; it does not put secret values in `.env`, Compose command arguments, or startup logs. Keep backups of `private/` and the database volume under your normal encrypted backup policy. Losing session encryption keys invalidates existing client sessions.

## Validate and start

First validate the local inputs. The command checks all seven GameFleet fields, source-file type/permissions, exact network addresses, the image identity and required module files, and the Compose model. It does not display resolved env values or key contents:

```sh
sudo python3 /opt/nakama-source/scripts/nakama_stack.py validate --directory /opt/nakama
sudo python3 /opt/nakama-source/scripts/nakama_stack.py plan --directory /opt/nakama
```

If using a registry image, pull it by the exact digest before `validate`:

```sh
sudo docker pull 'registry.example/repository@sha256:<64-hex-digest>'
```

On the deployment host, resolve Compose without printing the model, pull the pinned public dependencies, build the tunnel sidecar, and then start the stack:

```sh
sudo docker compose --env-file /opt/nakama/.env --project-directory /opt/nakama \
  -f /opt/nakama/compose.yaml config --quiet
sudo docker compose --env-file /opt/nakama/.env --project-directory /opt/nakama \
  -f /opt/nakama/compose.yaml pull postgres caddy
sudo docker compose --env-file /opt/nakama/.env --project-directory /opt/nakama \
  -f /opt/nakama/compose.yaml build --pull gamefleet-tunnel
sudo docker compose --env-file /opt/nakama/.env --project-directory /opt/nakama \
  -f /opt/nakama/compose.yaml up -d
```

This `up` command is the deployment action. The helper has no `apply`, remote SSH, or public-registry push command. Compose starts the local SSH sidecar in Nakama's network namespace; the Nakama entrypoint waits for the local tunnel listener, runs Nakama's database migration, then starts the server.

## Verify actual operation

Check service process state and inspect logs locally:

```sh
sudo docker compose --env-file /opt/nakama/.env --project-directory /opt/nakama \
  -f /opt/nakama/compose.yaml ps
sudo docker compose --env-file /opt/nakama/.env --project-directory /opt/nakama \
  -f /opt/nakama/compose.yaml logs --tail=200 nakama gamefleet-tunnel caddy postgres
```

Confirm the TLS certificate for `NAKAMA_DOMAIN`, successful Nakama startup and migration, a healthy tunnel, and the adapter's service/search/History preflight. Test account login, matching, reconnect/resume, and GameFleet grants with real client flows. Compose `ps`, a green TCP tunnel healthcheck, and a Caddy certificate do not prove the service grants, account provider, room lifecycle, or production availability.

The Nakama Console is not proxied publicly. To reach it from an operator workstation, use an SSH local forward to the Nakama host's loopback-only port:

```sh
ssh -N -L 17351:127.0.0.1:17351 operator@nakama-host
```

Then open `http://127.0.0.1:17351` locally and use username `admin` and the generated console password from its private file. Do not add a public Compose port mapping for 7350, 7351, 7348, 7349, or 17682.

## Recovery and limits

Back up PostgreSQL and the `private/` key files together. Protect Caddy's data volume so TLS renewal state survives container replacement. Test restore before production use. Do not regenerate the database password independently after PostgreSQL initialization; rotate it inside PostgreSQL and update the private file as one controlled change. Do not regenerate Nakama session keys during a normal image rollout.

This stack is a single-host deployment and is not highly available. It does not install GameFleet, create service identities/grants, build the Fixed application image, provision DNS/firewall policy, or establish a tested backup. Those remain explicit owner steps. For the adapter's request contract and separate authorization scopes, see [service runtime](gamefleet-service-runtime.md); for component and port ownership, see [architecture](architecture.md).
