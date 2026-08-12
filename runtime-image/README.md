# Runtime image

This directory owns the immutable multi-JRE image used by the node agent. The
image, node agent, quota helper, and fixed runtime entrypoint are released from
the same repository commit.

`runtime-catalog.lock.json` pins the official Mojang Java assets.
`reviewed-toolchains.json` pins the Minecraft/loader tuples accepted by the
agent. `scripts/sync_agent_catalog.py` combines them into
`internal/runtime/catalog.json`; CI rejects a stale generated catalog.

The release workflow builds `cmd/xmcl-shared-minecraft-runtime` from the same
commit, downloads and verifies BusyBox from `runtime-assets.lock.json`,
materializes every checksum-pinned JRE, and builds `Dockerfile` from those
inputs. Nothing is downloaded when a customer container starts.

Local validation:

```sh
python3 -m unittest discover -s runtime-image/tests -v
python3 runtime-image/scripts/validate_runtime_assets.py
python3 runtime-image/scripts/validate_runtime_catalog.py
python3 runtime-image/scripts/sync_agent_catalog.py --check
go test ./...
```
