# Compatibility

The plugin must match the host's Go compiler, shared dependencies and build settings. This follows [Go plugin requirements](https://pkg.go.dev/plugin) and [Nakama dependency requirements](https://heroiclabs.com/docs/nakama/server-framework/go-runtime/go-dependencies/).

| Component | Target |
| --- | --- |
| Official Nakama and pluginbuilder | 3.41.0 |
| Go | 1.27.1 |
| nakama-common | v1.48.0 |
| pgx/v5 | v5.11.0 |
| protobuf | v1.36.12 |
| Runtime plugin | linux/amd64 |
| Agones | 1.60.0 |
| K3s (local and Linux node installer) | v1.35.8+k3s1 |
| Local k3d / Helm | 5.9.0 / 3.22.0 |

Exact image digests and shared Go modules are in `deploy/compatibility.env`, `go.mod` and `scripts/check-compatibility.sh`. The Docker build checks them before compiling `agones.so`. See the validation record for executed runtime tests. Changing only a tag is not an upgrade procedure.

```sh
make build
make package
```

The resulting release archive contains `agones.so`, the independent `fleet-migrate` executable, compatibility metadata and checksums. You may use the derived runtime image, or mount the matching module read-only at `/nakama/data/modules/agones.so` in the exact official Nakama image. Both require separate Nakama/fleet database migrations and the documented environment. The source library entry point is `github.com/xuhuanhello/nakama-agones/pkg/fleetmanager.RegisterFromEnv`.

This standalone module registers the default FleetManager and matchmaking hooks. The API also supports named managers, but using several requires an application that explicitly composes routing/hooks; loading competing standalone modules is unsupported.

Apple Silicon local testing uses the ARM K3s/Agones stack and an emulated amd64 Nakama image. Agones documents ARM support as alpha. Production DM targets Linux amd64. Portable Unity tests and protocol fixtures are not performance benchmarks.

## CI image source and rate limits

The pinned official images use `docker.io/heroiclabs/nakama` and
`docker.io/heroiclabs/nakama-pluginbuilder`. Only the registry address changed;
both `3.41.0` tags and SHA256 digests remain identical. Heroic Labs documents that
[its registry gateway redirects to the hosting registry](https://heroiclabs.com/blog/docker-hosting-update/),
so switching the gateway hostname alone does not avoid Docker Hub's anonymous quota.

The legacy integration job configures the **disposable GitHub-hosted runner** to
prefer Google's `https://mirror.gcr.io` Docker Hub cache before creating k3d.
This covers ordinary pulls and the Docker driver's built-in BuildKit. It preserves
other daemon settings and refuses this setup step on a self-hosted runner. It does
not modify production VPS configuration or need a registry token. Docker verifies
the pinned content digests irrespective of which endpoint serves the image.

[Google documents that cache hits do not consume Docker Hub pull quota](https://cloud.google.com/artifact-registry/docs/pull-cached-dockerhub-images).
Cache entries may expire; on a cache miss Docker falls back to Docker Hub. This is
an independent cache, not an unlimited-availability guarantee. If both endpoints
fail, keep the pinned digests and inspect the registry error; do not downgrade the
Nakama version or substitute a plugin builder from a different release. A future
private mirror or authenticated Hub pull can cover persistent cache misses.

Local/self-hosted users may optionally merge `"registry-mirrors":
["https://mirror.gcr.io"]` into their own Docker daemon configuration using the
Google instructions. Restart that daemon only during their own maintenance window;
`local_cluster.py` never changes or restarts the host Docker service itself.
