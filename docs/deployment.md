# Deploy Nakama and GameFleet service from a blank Linux host


This is the currently implemented Nakama deployment path: a full Compose stack with PostgreSQL, the complete application Nakama image, an HTTPS gateway, and direct mutually authenticated HTTPS to the dedicated GameFleet Business endpoint. The commands prepare and run the Nakama host. They do not install or modify the separate GameFleet platform.

The helper never receives a password argument. It generates local database and Nakama keys without printing them, validates file permissions from metadata, checks a local image and module files, and runs Compose's quiet model validation. It does not SSH, publish an image, start containers, or claim remote health.

## Requirements and owner-provided inputs

Use a Linux amd64 host with Docker Engine, the Docker Compose plugin, and Python 3.10 or later. Confirm the host has free TCP ports 80/443 and unused Docker subnets for the two bridges. The Nakama plugin ABI in this repository targets `linux/amd64` and the pinned toolchain in [compatibility](compatibility.md).

Before starting, obtain these inputs through the responsible owner systems:

- A GameFleet service identity, its private `gfsvc_` key, exact application/issuer/region/compatibility values, search and match grants, and History routes.
- A reachable dedicated GameFleet HTTPS business origin, its CA certificate, and an owner-issued client certificate/private key. Verify private routing when available; use public mTLS otherwise.
- A complete Nakama application image from Fixed's `Backend/deploy/account/build.py`. Build it on this Nakama host when using a local image ID, or publish it through your own registry and use the immutable repository digest. The image must contain `/nakama/data/modules/agones.so`; for Fixed account login, it must also contain `/nakama/data/modules/account.so`.
- A DNS name pointing at the Nakama host. Allow inbound public TCP 80/443 (and UDP 443 only if you want HTTP/3); restrict SSH administration and permit outbound HTTPS only to required endpoints according to your host policy.

The remote platform must expose only `/business/v1/*` on its dedicated mTLS HTTPS endpoint. Do not reuse administrator, upload, database or Kubernetes listeners.

## Create the host project

Obtain the source and pin the exact revision on the Nakama VPS. The working branch below contains this deployment entry; record the printed commit in your deployment record. For a selected release, checkout its reviewed commit instead.

```sh
sudo git clone --branch codex/self-service-bootstrap https://github.com/xuhuanhello/nakama-agones.git /opt/nakama-source
cd /opt/nakama-source
git rev-parse HEAD
sudo git checkout --detach "$(git rev-parse HEAD)"
```

Run initialization, validation, and Compose with host administrator privileges. The stack directory and private credential files belong to root:

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

Set `NAKAMA_DOMAIN` to the public DNS name. Keep the default Docker networks only if they do not overlap any existing networks or routed VPN ranges; otherwise choose two non-overlapping IPv4 subnets and distinct static frontend addresses. The default Caddy address is `172.29.240.2` and Nakama is `172.29.240.3`.

Copy the owner-issued service key to `/opt/nakama/private/gamefleet-service-key`. Set the seven exact service fields in `/opt/nakama/private/gamefleet-service.env`; its in-container key path must remain `/run/secrets/gamefleet-service-key`. Set `GAMEFLEET_SERVICE_URL` to the dedicated HTTPS business origin, using its certificate SAN hostname. Set every identity and profile field to the value provisioned by the GameFleet owner; do not infer values from the image tag.

Install the platform-issued CA certificate, client certificate and client private key as root-owned regular files with mode `0400`, at the configured `GAMEFLEET_CA_SOURCE`, `GAMEFLEET_CERT_SOURCE` and `GAMEFLEET_TLS_KEY_SOURCE` paths. Defaults are `private/gamefleet-ca.crt`, `private/gamefleet-client.crt`, and `private/gamefleet-client.key`. The service template maps these to `/run/secrets/gamefleet-ca.crt`, `/run/secrets/gamefleet-client.crt`, and `/run/secrets/gamefleet-client.key`. Never copy the platform CA signing key to Nakama.

For rotation, obtain new leaves from the same trusted authority before expiry, replace the host leaf files atomically, then recreate the Nakama container and verify its authenticated service preflight and player flows. A CA change requires coordinated trust overlap and validation. `init` never generates a substitute for owner-issued service identities. An enabled archive reader has its own three `GAMEFLEET_ARCHIVE_*_SOURCE` files and explicit `_CA_FILE`, `_CERT_FILE`, `_TLS_KEY_FILE` targets; it never inherits the active identity.

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

On the deployment host, resolve Compose without printing the model, pull the pinned public dependencies, and then start the stack:

```sh
sudo docker compose --env-file /opt/nakama/.env --project-directory /opt/nakama \
  -f /opt/nakama/compose.yaml config --quiet
sudo docker compose --env-file /opt/nakama/.env --project-directory /opt/nakama \
  -f /opt/nakama/compose.yaml pull postgres caddy
sudo docker compose --env-file /opt/nakama/.env --project-directory /opt/nakama \
  -f /opt/nakama/compose.yaml up -d
```

This `up` command is the deployment action. The helper has no `apply`, remote SSH, or public-registry push command. The Nakama entrypoint runs database migrations and starts the server; plugin initialization performs real authenticated HTTPS search and History scope checks before registering player hooks.

## Verify actual operation

Check service process state and inspect logs locally:

```sh
sudo docker compose --env-file /opt/nakama/.env --project-directory /opt/nakama \
  -f /opt/nakama/compose.yaml ps
sudo docker compose --env-file /opt/nakama/.env --project-directory /opt/nakama \
  -f /opt/nakama/compose.yaml logs --tail=200 nakama caddy postgres
```

Confirm the TLS certificate for `NAKAMA_DOMAIN`, successful Nakama startup and migration, business DNS/route/TLS reachability, and the adapter's service/search/History preflight. Test account login, matching, reconnect/resume, and GameFleet grants with real client flows. Compose `ps`, and a Caddy certificate do not prove the service grants, account provider, room lifecycle, or production availability.

The Nakama Console is not proxied publicly. To reach it from an operator workstation, use an SSH local forward to the Nakama host's loopback-only port:

```sh
ssh -N -L 17351:127.0.0.1:17351 operator@nakama-host
```

Then open `http://127.0.0.1:17351` locally and use username `admin` and the generated console password from its private file. Do not add a public Compose port mapping for 7350, 7351, 7348, 7349, or 17682.

## Recovery and limits

Back up PostgreSQL and the `private/` key files together. Protect Caddy's data volume so TLS renewal state survives container replacement. Test restore before production use. Do not regenerate the database password independently after PostgreSQL initialization; rotate it inside PostgreSQL and update the private file as one controlled change. Do not regenerate Nakama session keys during a normal image rollout.

This stack is a single-host deployment and is not highly available. It does not install GameFleet, create service identities/grants, build the Fixed application image, provision DNS/firewall policy, or establish a tested backup. Those remain explicit owner steps. For the adapter's request contract and separate authorization scopes, see [service runtime](gamefleet-service-runtime.md); for component and port ownership, see [architecture](architecture.md).
