// CI-only: download only reviewed catalog artifacts OUTSIDE the offline Builder.
import { readFile, mkdir, chmod, writeFile } from "node:fs/promises";
import { resolve } from "node:path";
import { ReviewedToolchainCatalog, StrictArtifactDownloader } from "../src/reviewed-builder.mjs";
import { validateBuildSpecV1, buildKeyFor } from "../src/build-spec.mjs";

const root = process.env.XMCL_BUILDER_PROBE_ROOT;
const image = process.env.XMCL_BUILDER_DOCKER_TEST_IMAGE;
const runtime = process.env.XMCL_BUILDER_PROBE_RUNTIME_IMAGE;
if (!root || resolve(root) !== root || !/^ghcr\.io\/voxelum\/xmcl-shared-node-builder@sha256:[a-f0-9]{64}$/.test(image ?? "") ||
    !/^ghcr\.io\/voxelum\/xmcl-shared-minecraft-runtime@sha256:[a-f0-9]{64}$/.test(runtime ?? "")) {
  throw new Error("explicit absolute probe root and immutable published images required");
}
const fixture = JSON.parse(await readFile(new URL("../test/fixtures/neoforge-1.21.1-agent-builder-v1.json", import.meta.url), "utf8"));
const catalog = new ReviewedToolchainCatalog(JSON.parse(await readFile(new URL("../toolchain-catalog.lock.json", import.meta.url), "utf8")), { supportedFamilies: ["neoforge"] });
const spec = structuredClone(fixture.buildSpec);
spec.builder.image = image;
spec.runtime.image = runtime;
const toolchain = catalog.resolve({ runtimeCatalogRevision: spec.runtime.runtimeCatalogRevision, ...spec.compatibility });
const kind = a => a.role === "primary" ? "toolchain" : a.coordinate.startsWith("com.mojang:minecraft-server:") ? "minecraft" : a.coordinate.startsWith("net.minecraft:server:") && a.coordinate.endsWith(":mappings") ? "mapping" : "library";
spec.dependencies = toolchain.artifacts.map(a => ({ id: a.coordinate, kind: kind(a), sha256: a.sha256, sizeBytes: a.sizeBytes, targetPath: `toolchain/${a.sha256}.artifact` }))
  .sort((a, b) => Buffer.compare(Buffer.from(`${a.kind}\0${a.id}\0${a.targetPath}`), Buffer.from(`${b.kind}\0${b.id}\0${b.targetPath}`)));
validateBuildSpecV1(spec);
await mkdir(root); // Refuse reuse/overwrite of a prior probe.
await chmod(root, 0o755);
await mkdir(resolve(root, "dependencies"), { mode: 0o755 });
await mkdir(resolve(root, "output"), { mode: 0o777 });
await chmod(resolve(root, "output"), 0o777);
const downloader = new StrictArtifactDownloader({ fetchImpl: fetch, timeoutMs: 300_000 });
const mounts = [];
for (const [i, dep] of spec.dependencies.entries()) {
  const artifact = toolchain.artifacts.find(a => a.coordinate === dep.id);
  const data = await downloader.download(artifact, { approvedHosts: catalog.approvedArtifactHosts });
  const path = `${String(i).padStart(3, "0")}-${dep.sha256}`;
  await writeFile(resolve(root, "dependencies", path), data, { mode: 0o444, flag: "wx" });
  mounts.push({ id: dep.id, path, sha256: dep.sha256, sizeBytes: dep.sizeBytes });
}
const zip = Buffer.alloc(22); zip.writeUInt32LE(0x06054b50);
await writeFile(resolve(root, "source.pack"), zip, { mode: 0o444, flag: "wx" });
await writeFile(resolve(root, "build-spec.json"), JSON.stringify(spec), { mode: 0o444, flag: "wx" });
await writeFile(resolve(root, "dependencies.json"), JSON.stringify({ schemaVersion: 1, dependencies: mounts }), { mode: 0o444, flag: "wx" });
console.log(JSON.stringify({ artifacts: mounts.length, buildKey: buildKeyFor(spec), source: "empty ZIP", loader: spec.compatibility.loader, offlineContainer: true }));
