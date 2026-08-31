import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { chmod, mkdir, rm, writeFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import test from "node:test";
import {
  FilesystemReplayStore,
  HmacServiceIdentity,
} from "../src/service-identity.mjs";
import { VerifiedReadOnlyJreRegistry } from "../src/verified-jre-registry.mjs";
import { BubblewrapSandboxAdapter } from "../src/bubblewrap-sandbox.mjs";
import { canonicalJsonBytes } from "../src/toolchain-catalog.mjs";
import { safeSecretMetadata } from "../src/production-composition.mjs";

const encoder = new TextEncoder();
const testRoot = resolve(".test-production-adapters");

test.after(async () => {
  await rm(testRoot, { recursive: true, force: true });
});

test("production identity accepts only private source or systemd credential modes", () => {
  const metadata = (mode, uid = 0, gid = 0) => ({
    mode,
    uid,
    gid,
    size: 64,
    isFile: () => true,
  });
  assert.equal(safeSecretMetadata(metadata(0o100400)), true);
  assert.equal(safeSecretMetadata(metadata(0o100440)), true);
  assert.equal(safeSecretMetadata(metadata(0o100440, 10001, 10001)), false);
  assert.equal(safeSecretMetadata(metadata(0o100444)), false);
  assert.equal(safeSecretMetadata(metadata(0o100600)), false);
});

test("HMAC production identity uses atomic durable replay protection", async () => {
  const replayDirectory = join(testRoot, "replay");
  const store = new FilesystemReplayStore({ directory: replayDirectory });
  const identity = new HmacServiceIdentity({
    keyId: "compiler-staging",
    secret: "shared-production-secret-at-least-32-bytes",
    nonceStore: store,
    clock: () => 20_000,
    requireDurableReplay: true,
  });
  assert.equal(identity.replayProtected, true);
  assert.equal(store.durable, true);
  const body = encoder.encode('{"schemaVersion":1}');
  const headers = await identity.signOutgoing({
    method: "POST",
    target: "/v1/compiler-jobs",
    body,
  });
  await identity.verifyIncoming({
    method: "POST",
    target: "/v1/compiler-jobs",
    headers,
    body,
  });
  await assert.rejects(() => identity.verifyIncoming({
    method: "POST",
    target: "/v1/compiler-jobs",
    headers,
    body,
  }), /request_replayed/);
});

test("JRE registry binds every root to exact reviewed catalog identities", async () => {
  const jreRoot = join(testRoot, "jres");
  const root = join(jreRoot, "java-runtime-delta-21");
  await mkdir(join(root, "bin"), { recursive: true });
  const java = join(root, "bin", "java");
  await writeFile(java, "java");
  await chmod(java, 0o555);
  const javaBytes = encoder.encode("java");
  const resolution = {
    component: "java-runtime-delta",
    major: 21,
    files: [
      { path: "bin", type: "directory" },
      {
        path: "bin/java",
        type: "file",
        executable: true,
        size: javaBytes.byteLength,
        hashes: { sha1: createHash("sha1").update(javaBytes).digest("hex") },
      },
    ],
  };
  await writeFile(join(root, ".xmcl-runtime-resolution.json"), JSON.stringify(resolution));
  const jre = {
    id: "java-runtime-delta-21",
    sha256: createHash("sha256").update(canonicalJsonBytes(resolution)).digest("hex"),
    component: "java-runtime-delta",
    major: 21,
    runtimeCatalogRevision: "b".repeat(64),
  };
  const catalog = {
    catalogRevision: "c".repeat(64),
    runtimeCatalogRevision: jre.runtimeCatalogRevision,
    toolchains: [{ jre }],
  };
  const document = {
    schemaVersion: 1,
    catalogRevision: catalog.catalogRevision,
    runtimeCatalogRevision: catalog.runtimeCatalogRevision,
    roots: [{ ...jre, path: root }],
  };
  const registry = new VerifiedReadOnlyJreRegistry({
    document,
    catalog,
    rootDirectory: jreRoot,
    mountInspector: async () => true,
  });
  const resolved = await registry.resolve(jre);
  assert.equal(resolved.verified, true);
  assert.equal(resolved.readOnly, true);
  assert.equal(registry.rootForToken(resolved.rootToken), root);
  assert.throws(() => new VerifiedReadOnlyJreRegistry({
    document: {
      ...document,
      roots: [{ ...document.roots[0], sha256: "d".repeat(64) }],
    },
    catalog,
    rootDirectory: jreRoot,
  }), /invalid verified JRE registry/);
});

test("bubblewrap adapter assembles exact NeoForge inputs with enforced isolation", async () => {
  const workspaceRoot = join(testRoot, "workspaces");
  const jreRoot = join(testRoot, "sandbox-jre");
  const commands = [];
  const adapter = new BubblewrapSandboxAdapter({
    jreRegistry: { rootForToken: () => jreRoot },
    workspaceRoot,
    bubblewrapPath: resolve("bwrap"),
    prlimitPath: resolve("prlimit"),
    installerRewriter: (value) => value,
    runner: async (command, args, options) => {
      commands.push({ command, args, options });
      const bindIndex = args.findIndex((item, index) =>
        item === "--bind" && args[index + 2] === "/work");
      const output = args[bindIndex + 1];
      const unixArgs = join(output, "libraries", "net", "neoforged", "neoforge",
        "21.1.115", "unix_args.txt");
      await mkdir(resolve(unixArgs, ".."), { recursive: true });
      await writeFile(unixArgs, "-jar libraries/neoforge-server.jar\n");
    },
  });
  const artifacts = [
    {
      coordinate: "net.neoforged:neoforge:21.1.115:installer",
      bytes: encoder.encode("exact-installer"),
    },
    {
      coordinate: "com.mojang:minecraft-server:1.21.1:server",
      bytes: encoder.encode("exact-server"),
    },
    {
      coordinate: "net.neoforged:neoforge:21.1.115:universal",
      url: "https://maven.neoforged.net/releases/net/neoforged/neoforge/21.1.115/neoforge-21.1.115-universal.jar",
      bytes: encoder.encode("exact-dependency"),
    },
  ];
  const result = await adapter.assemble({
    schemaVersion: 1,
    sandbox: {
      ephemeralWorkspace: true,
      nonRoot: true,
      readOnlyBaseFilesystem: true,
      noSecrets: true,
      noDockerSocket: true,
      boundedResources: true,
    },
    jre: { rootToken: "jre-token" },
    plan: {
      family: "neoforge",
      minecraftVersion: "1.21.1",
      loader: { version: "21.1.115" },
    },
    artifacts,
  });
  assert.equal(commands.length, 1);
  const command = commands[0];
  assert.equal(command.options.env && Object.keys(command.options.env).length, 0);
  assert.ok(command.args.includes("--unshare-all"));
  assert.ok(command.args.includes("--clearenv"));
  assert.ok(command.args.includes("--tmpfs"));
  assert.ok(command.args.includes("--nproc=256"));
  assert.ok(command.args.includes("-XX:ActiveProcessorCount=2"));
  assert.ok(command.args.includes("-XX:CompressedClassSpaceSize=256m"));
  assert.ok(command.args.includes("-XX:ReservedCodeCacheSize=128m"));
  assert.ok(command.args.includes("--offline"));
  assert.ok(result.files.has("libraries/net/minecraft/server/1.21.1/server-1.21.1.jar"));
  assert.ok(result.files.has("libraries/net/neoforged/neoforge/21.1.115/unix_args.txt"));
  assert.deepEqual(result.attestation, {
    ephemeralWorkspace: true,
    nonRoot: true,
    readOnlyBaseFilesystem: true,
    noSecrets: true,
    noDockerSocket: true,
    boundedResources: true,
    network: "disabled",
  });
  assert.deepEqual(await readdirOrEmpty(workspaceRoot), []);
});

async function readdirOrEmpty(path) {
  const { readdir } = await import("node:fs/promises");
  return await readdir(path).catch(() => []);
}
