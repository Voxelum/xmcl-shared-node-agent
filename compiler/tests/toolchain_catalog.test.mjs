import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";
import { ReviewedToolchainCatalog } from "../src/reviewed-builder.mjs";
import {
  APPROVED_ARTIFACT_HOSTS,
  catalogRevisionFor,
  formatCatalog,
  generateToolchainCatalog,
  primaryArtifactUrl,
  StrictCatalogFetcher,
  SUPPORTED_TOOLCHAIN_CANDIDATES,
  validateToolchainCatalog,
} from "../src/toolchain-catalog.mjs";

const encoder = new TextEncoder();

function bytes(value) {
  return encoder.encode(value);
}

function sha1(value) {
  return createHash("sha1").update(value).digest("hex");
}

function runtimeCatalog(candidates = SUPPORTED_TOOLCHAIN_CANDIDATES) {
  const java = [...new Map(candidates.map((candidate) => [
    `${candidate.java.component}\0${candidate.java.major}`,
    candidate.java,
  ])).values()];
  return bytes(JSON.stringify({
    schemaVersion: 1,
    platform: { architecture: "x64", os: "linux" },
    requirements: java,
    runtimes: java,
    resolutions: java.map((entry) => ({
      component: entry.component,
      major: entry.major,
      source: "mojang",
      manifest: { sha1: sha1(bytes(`${entry.component}:${entry.major}`)) },
    })),
  }));
}

function fixtureNetwork(candidates = SUPPORTED_TOOLCHAIN_CANDIDATES) {
  const responses = new Map();
  const versions = [];
  for (const candidate of candidates) {
    const versionUrl = `https://piston-meta.mojang.com/versions/${candidate.minecraftVersion}.json`;
    const serverBytes = bytes(`server:${candidate.minecraftVersion}`);
    const serverSha1 = sha1(serverBytes);
    const metadata = {
      downloads: {
        server: {
          url: `https://piston-data.mojang.com/v1/objects/${serverSha1}/server.jar`,
          sha1: serverSha1,
          size: serverBytes.byteLength,
        },
      },
    };
    if (candidate.java.component !== "jre-legacy") {
      metadata.javaVersion = {
        component: candidate.java.component,
        majorVersion: candidate.java.major,
      };
    }
    const metadataBytes = bytes(JSON.stringify(metadata));
    versions.push({ id: candidate.minecraftVersion, url: versionUrl, sha1: sha1(metadataBytes) });
    responses.set(versionUrl, response(metadataBytes));
    responses.set(metadata.downloads.server.url, response(serverBytes));

    const primary = primaryArtifactUrl(candidate);
    const primaryBytes = bytes(`primary:${candidate.loader.kind}:${candidate.loader.version}`);
    responses.set(primary, response(primaryBytes));
    if (candidate.loader.kind === "fabric") {
      const url = `https://meta.fabricmc.net/v2/versions/loader/${candidate.minecraftVersion}/${candidate.loader.version}`;
      responses.set(url, response(bytes(JSON.stringify({
        loader: {
          version: candidate.loader.version,
          maven: `net.fabricmc:fabric-loader:${candidate.loader.version}`,
        },
        intermediary: { version: candidate.minecraftVersion },
      }))));
      responses.set(`${primary}.sha1`, response(bytes(sha1(primaryBytes))));
    } else if (candidate.loader.kind === "quilt") {
      const url = `https://meta.quiltmc.org/v3/versions/loader/${candidate.minecraftVersion}/${candidate.loader.version}`;
      responses.set(url, response(bytes(JSON.stringify({
        loader: {
          version: candidate.loader.version,
          maven: `org.quiltmc:quilt-loader:${candidate.loader.version}`,
          file_size: primaryBytes.byteLength,
          hashes: { sha1: sha1(primaryBytes) },
        },
        intermediary: { version: candidate.minecraftVersion },
      }))));
    } else {
      const forge = candidate.loader.kind === "forge";
      const metadataUrl = forge
        ? "https://maven.minecraftforge.net/net/minecraftforge/forge/maven-metadata.xml"
        : "https://maven.neoforged.net/releases/net/neoforged/neoforge/maven-metadata.xml";
      const groupId = forge ? "net.minecraftforge" : "net.neoforged";
      const artifactId = forge ? "forge" : "neoforge";
      const version = forge
        ? `${candidate.minecraftVersion}-${candidate.loader.version}`
        : candidate.loader.version;
      responses.set(metadataUrl, response(bytes(
        `<?xml version="1.0" encoding="UTF-8"?><metadata><groupId>${groupId}</groupId>` +
        `<artifactId>${artifactId}</artifactId><versioning><versions><version>${version}</version>` +
        "</versions></versioning></metadata>",
      )));
      responses.set(`${primary}.sha1`, response(bytes(sha1(primaryBytes))));
    }
  }
  responses.set(
    "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json",
    response(bytes(JSON.stringify({ versions }))),
  );
  return {
    responses,
    fetch: async (url, options) => {
      assert.equal(options.method, "GET");
      assert.equal(options.redirect, "error");
      assert.equal(options.credentials, "omit");
      const value = responses.get(url);
      if (!value) throw new Error(`unexpected URL ${url}`);
      return typeof value === "function" ? value() : value.clone();
    },
  };
}

function response(body, {
  status = 200,
  headers = { "content-length": String(body.byteLength) },
} = {}) {
  return new Response(body, { status, headers });
}

async function generatedFixture(candidates = SUPPORTED_TOOLCHAIN_CANDIDATES) {
  const runtime = runtimeCatalog(candidates);
  const network = fixtureNetwork(candidates);
  const catalog = await generateToolchainCatalog({
    runtimeCatalogBytes: runtime,
    fetchImpl: network.fetch,
    candidates,
  });
  return { catalog, network, runtime };
}

test("official Forge, Fabric, NeoForge, and Quilt metadata fixtures generate exact deterministic coordinates", async () => {
  const quilt = {
    minecraftVersion: "1.21.8",
    loader: { kind: "quilt", version: "0.28.0" },
    java: { component: "java-runtime-delta", major: 21 },
  };
  const candidates = [...SUPPORTED_TOOLCHAIN_CANDIDATES, quilt];
  const first = await generatedFixture(candidates);
  const second = await generatedFixture(candidates);
  assert.deepEqual(first.catalog, second.catalog);
  assert.deepEqual(first.catalog.approvedArtifactHosts, APPROVED_ARTIFACT_HOSTS);
  assert.deepEqual(
    first.catalog.toolchains.map((toolchain) => toolchain.artifacts.find((artifact) =>
      artifact.role === "primary",
    ).coordinate),
    [
      "net.minecraftforge:forge:1.12.2-14.23.5.2859:installer",
      "net.fabricmc:fabric-loader:0.12.12",
      "net.fabricmc:fabric-loader:0.15.11",
      "net.neoforged:neoforge:21.1.115:installer",
      "org.quiltmc:quilt-loader:0.28.0",
      "net.fabricmc:fabric-loader:0.19.3",
    ],
  );
  assert.equal(first.catalog.catalogRevision, catalogRevisionFor(first.catalog));
  assert.equal(
    formatCatalog(first.catalog),
    formatCatalog(JSON.parse(formatCatalog(first.catalog))),
  );
  validateToolchainCatalog(first.catalog, first.runtime, {
    expectedCandidates: candidates,
  });
});

test("generated catalog is accepted by the reviewed exact-match catalog, including Java 25", async () => {
  const { catalog } = await generatedFixture();
  const reviewed = new ReviewedToolchainCatalog(catalog);
  const current = reviewed.resolve({
    runtimeCatalogRevision: catalog.runtimeCatalogRevision,
    minecraftVersion: "26.2",
    loader: { kind: "fabric", version: "0.19.3" },
    java: { component: "java-runtime-epsilon", major: 25 },
  });
  assert.equal(current.jre.component, "java-runtime-epsilon");
  assert.equal(current.jre.major, 25);
  assert.throws(() => reviewed.resolve({
    runtimeCatalogRevision: catalog.runtimeCatalogRevision,
    minecraftVersion: "26.2",
    loader: { kind: "fabric", version: "0.19.4" },
    java: { component: "java-runtime-epsilon", major: 25 },
  }), /unsupported_compatibility/);
});

test("catalog generation rejects redirect, foreign host, missing or mismatched checksums, size mismatch, malformed metadata, unsafe versions, and duplicate tuples", async () => {
  const forge = SUPPORTED_TOOLCHAIN_CANDIDATES.filter((candidate) => candidate.loader.kind === "forge");
  const primary = primaryArtifactUrl(forge[0]);
  const cases = [
    {
      name: "redirect",
      arrange(network) {
        network.responses.set(primary, () => ({
          status: 200,
          redirected: true,
          headers: new Headers({ "content-length": "1" }),
        }));
      },
      expected: /upstream_fetch_failed/,
    },
    {
      name: "missing checksum",
      arrange(network) {
        network.responses.set(`${primary}.sha1`, response(bytes("not-a-checksum")));
      },
      expected: /upstream_checksum_missing/,
    },
    {
      name: "checksum mismatch",
      arrange(network) {
        network.responses.set(`${primary}.sha1`, response(bytes("0".repeat(40))));
      },
      expected: /upstream_checksum_mismatch/,
    },
    {
      name: "size mismatch",
      arrange(network) {
        const body = bytes("short");
        network.responses.set(primary, response(body, {
          headers: { "content-length": String(body.byteLength + 1) },
        }));
      },
      expected: /upstream_size_mismatch/,
    },
    {
      name: "malformed metadata",
      arrange(network) {
        network.responses.set(
          "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json",
          response(bytes("{")),
        );
      },
      expected: /minecraft_metadata_malformed/,
    },
    {
      name: "malformed Maven XML",
      arrange(network) {
        network.responses.set(
          "https://maven.minecraftforge.net/net/minecraftforge/forge/maven-metadata.xml",
          response(bytes("<!DOCTYPE metadata>")),
        );
      },
      expected: /maven_metadata_malformed/,
    },
  ];
  for (const scenario of cases) {
    const runtime = runtimeCatalog(forge);
    const network = fixtureNetwork(forge);
    scenario.arrange(network);
    await assert.rejects(
      () => generateToolchainCatalog({ runtimeCatalogBytes: runtime, fetchImpl: network.fetch, candidates: forge }),
      scenario.expected,
      scenario.name,
    );
  }
  const foreign = new StrictCatalogFetcher({ fetchImpl: async () => assert.fail("must not fetch") });
  await assert.rejects(
    () => foreign.artifact("https://unreviewed.example/file.jar"),
    /upstream_url_rejected/,
  );
  await assert.rejects(
    () => generateToolchainCatalog({
      runtimeCatalogBytes: runtimeCatalog(forge),
      fetchImpl: fixtureNetwork(forge).fetch,
      candidates: [{ ...forge[0], loader: { kind: "forge", version: "../unsafe" } }],
    }),
    /candidate_invalid/,
  );
  for (const minecraftVersion of [
    " 26.2",
    "26.2 ",
    "26.02",
    "../26.2",
    "https://example.test/26.2",
    "26.2;cmd",
    "26.2\n",
  ]) {
    await assert.rejects(
      () => generateToolchainCatalog({
        runtimeCatalogBytes: runtimeCatalog(forge),
        fetchImpl: fixtureNetwork(forge).fetch,
        candidates: [{ ...forge[0], minecraftVersion }],
      }),
      /candidate_invalid/,
      minecraftVersion,
    );
  }
  await assert.rejects(
    () => generateToolchainCatalog({
      runtimeCatalogBytes: runtimeCatalog(forge),
      fetchImpl: fixtureNetwork(forge).fetch,
      candidates: [forge[0], structuredClone(forge[0])],
    }),
    /duplicate_toolchain/,
  );
});

test("catalog validation rejects runtime mismatch, unknown templates, and missing required loader artifacts", async () => {
  const { catalog, runtime } = await generatedFixture();
  const runtimeMismatch = structuredClone(catalog);
  runtimeMismatch.toolchains[0].jre.component = "other-runtime";
  runtimeMismatch.catalogRevision = catalogRevisionFor(runtimeMismatch);
  assert.throws(
    () => validateToolchainCatalog(runtimeMismatch, runtime),
    /runtime_java_mismatch/,
  );

  const unknownTemplate = structuredClone(catalog);
  unknownTemplate.toolchains[0].launchTemplate = "unknown-template";
  unknownTemplate.catalogRevision = catalogRevisionFor(unknownTemplate);
  assert.throws(
    () => validateToolchainCatalog(unknownTemplate, runtime),
    /toolchain_malformed/,
  );

  const missingArtifact = structuredClone(catalog);
  missingArtifact.toolchains[0].artifacts.pop();
  missingArtifact.catalogRevision = catalogRevisionFor(missingArtifact);
  assert.throws(
    () => validateToolchainCatalog(missingArtifact, runtime),
    /toolchain_malformed/,
  );
});

test("runtime revision and Java component/major must match the reviewed runtime lock", async () => {
  const { catalog, runtime } = await generatedFixture();
  const alteredRuntime = new Uint8Array(runtime);
  alteredRuntime[alteredRuntime.byteLength - 2] ^= 1;
  assert.throws(
    () => validateToolchainCatalog(catalog, alteredRuntime),
    /runtime_catalog_malformed|catalog_revision_mismatch/,
  );
});
