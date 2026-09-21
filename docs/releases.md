# Independent releases

Changes are validated by Go/race checks and a real local K3s/Agones integration run. Use `make package` for a source-revision-labelled binary archive with compatibility metadata and SHA256SUMS.

The tag workflow runs CI before publishing `ghcr.io/<owner>/nakama-agones` and `ghcr.io/<owner>/nakama-agones-tools`, plus the binary archive. Use a version tag matching `PLUGIN_VERSION` in `deploy/compatibility.env`. Source and Unity releases are independent; do not tag the original PlayFlow repositories. Configure new GHCR packages as public if anonymous pulls are intended, and verify anonymous digest pulls before using them in a public deployment guide.

Pin the runtime, builder and game image digests. Test upgrades on a separate pool and drain the old one. Package compatibility describes one tested combination, not a general Nakama version range. Never include `.local`, filled environment files, kubeconfig, game credentials or private DM artifacts in a release.

## Console companion artifacts

The plugin archive also includes `fleet-console` and `fleet-console-control`. These executables run as separate systemd services; they are not loaded as Nakama modules.

For a console-only upgrade, use the standalone bundle:

```sh
./scripts/package-console.sh
```

This pins the module's Go version, builds Linux amd64 static binaries, includes public service/config templates, log manifests, credential/export helpers and console documentation, then generates SHA256SUMS. It does not pull the Nakama image or rebuild `agones.so`. The manual package workflow builds this bundle independently; tagged releases attach it beside the plugin archive after release checks pass.

Install from a verified archive and follow [console deployment](console.md). Preserve the VPS's private configuration and local log PVCs across updates. Restart only the changed console/broker service; replacing the Go runtime plugin remains a separate maintenance operation.
