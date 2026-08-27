import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import {
  chmod,
  mkdir,
  readFile,
  rm,
  writeFile,
} from "node:fs/promises";
import { join, resolve } from "node:path";
import test from "node:test";
import { materializeVerifiedJre } from "../scripts/materialize_verified_jre.mjs";
import {
  ReviewedToolchainCatalog,
} from "../src/reviewed-builder.mjs";
import {
  canonicalJsonBytes,
  catalogRevisionFor,
} from "../src/toolchain-catalog.mjs";
import { VerifiedReadOnlyJreRegistry } from "../src/verified-jre-registry.mjs";

const root = resolve(".test-jre-materializer");
const encoder = new TextEncoder();

test.after(async () => {
  await rm(root, { recursive: true, force: true });
});

test("materializes and re-verifies the exact catalog-bound Java 21 root", async () => {
  await rm(root, { recursive: true, force: true });
  await mkdir(root, { recursive: true });
  const java = encoder.encode("reviewed-java-21");
  const javaSha1 = sha1(java);
  const resolution = {
    component: "java-runtime-delta",
    files: [
      { path: "bin", type: "directory" },
      {
        executable: true,
        hashes: { sha1: javaSha1 },
        path: "bin/java",
        size: java.byteLength,
        type: "file",
        url: `https://piston-data.mojang.com/v1/objects/${javaSha1}/java`,
      },
    ],
    major: 21,
  };
  const runtime = {
    platform: { architecture: "x64", os: "linux" },
    requirements: [{ component: resolution.component, major: resolution.major }],
    resolutions: [resolution],
    runtimes: [{ component: resolution.component, major: resolution.major }],
    schemaVersion: 1,
  };
  const runtimeBytes = encoder.encode(JSON.stringify(runtime));
  const runtimeRevision = sha256(runtimeBytes);
  const jre = {
    id: "java-runtime-delta-21",
    sha256: sha256(canonicalJsonBytes(resolution)),
    component: resolution.component,
    major: resolution.major,
    runtimeCatalogRevision: runtimeRevision,
  };
  const catalog = {
    approvedArtifactHosts: [
      "maven.neoforged.net",
      "piston-data.mojang.com",
    ],
    catalogRevision: "0".repeat(64),
    runtimeCatalogRevision: runtimeRevision,
    schemaVersion: 1,
    toolchains: [{
      artifacts: [
        {
          coordinate: "com.mojang:minecraft-server:1.21.1:server",
          role: "dependency",
          sha256: "a".repeat(64),
          sizeBytes: 1,
          url: "https://piston-data.mojang.com/v1/objects/" +
            `${"a".repeat(40)}/server.jar`,
        },
        {
          coordinate: "net.neoforged:neoforge:21.1.115:installer",
          role: "primary",
          sha256: "b".repeat(64),
          sizeBytes: 1,
          url: "https://maven.neoforged.net/releases/net/neoforged/neoforge/" +
            "21.1.115/neoforge-21.1.115-installer.jar",
        },
      ],
      java: { component: resolution.component, major: resolution.major },
      jre,
      launchTemplate: "neoforge-unix-args-v1",
      loader: { kind: "neoforge", version: "21.1.115" },
      minecraftVersion: "1.21.1",
    }],
  };
  catalog.catalogRevision = catalogRevisionFor(catalog);
  const runtimePath = join(root, "runtime-catalog.json");
  const catalogPath = join(root, "toolchain-catalog.json");
  const outputRoot = join(root, "jres", jre.id);
  await writeFile(runtimePath, runtimeBytes);
  await writeFile(catalogPath, JSON.stringify(catalog));

  const result = await materializeVerifiedJre({
    runtimeCatalogPath: runtimePath,
    toolchainCatalogPath: catalogPath,
    outputRoot,
    fetchImpl: async (url, options) => {
      assert.equal(url, resolution.files[1].url);
      assert.equal(options.redirect, "error");
      return new Response(java, {
        status: 200,
        headers: { "content-length": String(java.byteLength) },
      });
    },
  });
  assert.equal(result.sha256, jre.sha256);
  assert.equal(result.runtimeCatalogRevision, runtimeRevision);
  assert.deepEqual(new Uint8Array(await readFile(join(outputRoot, "bin", "java"))), java);
  const manifest = JSON.parse(await readFile(
    join(outputRoot, ".xmcl-runtime-resolution.json"), "utf8"));
  assert.equal(sha256(canonicalJsonBytes(manifest)), jre.sha256);

  const reviewed = new ReviewedToolchainCatalog(catalog, {
    supportedFamilies: ["neoforge"],
  });
  const registry = new VerifiedReadOnlyJreRegistry({
    document: {
      schemaVersion: 1,
      catalogRevision: reviewed.catalogRevision,
      runtimeCatalogRevision: reviewed.runtimeCatalogRevision,
      roots: [{ ...jre, path: outputRoot }],
    },
    catalog: reviewed,
    rootDirectory: join(root, "jres"),
    mountInspector: async () => true,
  });
  await chmod(join(outputRoot, "bin", "java"), 0o555);
  const verified = await registry.resolve(jre);
  assert.equal(verified.verified, true);
  assert.equal(verified.readOnly, true);
});

function sha1(bytes) {
  return createHash("sha1").update(bytes).digest("hex");
}

function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}
