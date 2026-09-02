import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import test from "node:test";
import {
  acaPathsFor,
  acquireExecutionLease,
  parseAcaDelivery,
  runAcaCompilerJob,
} from "../src/aca-job-runner.mjs";
import { CompilerHttpWorker } from "../src/http-worker.mjs";
import {
  decodeBase64,
  outerSandboxArguments,
} from "../scripts/aca_entrypoint.mjs";
import { bubblewrapProbeArguments } from "../scripts/probe_bubblewrap.mjs";
import {
  dockerBuildArguments,
  dockerRunArguments,
} from "../scripts/probe_bubblewrap_container.mjs";

const encoder = new TextEncoder();

test("ACA delivery is exact, bounded, and passed to one terminal worker attempt", async () => {
  const root = await mkdtemp(join(tmpdir(), "xmcl-aca-job-"));
  const path = join(root, "delivery.json");
  const body = encoder.encode('{"schemaVersion":1,"requestId":"request_1"}');
  const document = {
    schemaVersion: 1,
    method: "POST",
    target: "/v1/compiler-jobs",
    headers: {
      authorization: "HMAC compiler:test",
      "x-xmcl-nonce": "0123456789abcdef",
      "x-xmcl-timestamp": "1000",
    },
    bodyBase64: Buffer.from(body).toString("base64"),
  };
  await writeFile(path, JSON.stringify(document));
  const calls = [];
  const fakeWorker = {
    async initialize() {
      calls.push("initialize");
    },
    async consume(delivery) {
      calls.push(delivery);
      return { status: "accepted", deploymentId: "deployment_1" };
    },
    async runOne() {
      calls.push([...arguments]);
      return { terminal: true, result: { status: "published" } };
    },
  };
  try {
    const result = await runAcaCompilerJob({
      deliveryPath: path,
      workerFactory: async (options) => {
        assert.deepEqual(options, {
          paths: acaPathsFor("request_1"),
          startWorker: false,
        });
        return fakeWorker;
      },
      acquireLease: async (id) => {
        assert.equal(id, "request_1");
        return { release: async () => calls.push("release") };
      },
    });
    assert.equal(calls[0], "initialize");
    assert.deepEqual(calls[1].body, body);
    assert.deepEqual(calls[2], ["request_1"]);
    assert.equal(calls[3], "release");
    assert.equal(result.outcome.status, "published");
    await assert.rejects(() => runAcaCompilerJob({
      deliveryPath: path,
      workerFactory: async () => ({
        ...fakeWorker,
        runOne: async () => ({ terminal: false, result: { status: "queue_retry" } }),
      }),
      acquireLease: async () => ({ release: async () => undefined }),
    }), (error) => error?.code === "queue_retry");
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("ACA delivery rejects added fields and non-canonical base64", () => {
  const valid = {
    schemaVersion: 1,
    method: "POST",
    target: "/v1/compiler-jobs",
    headers: {
      authorization: "HMAC compiler:test",
      "x-xmcl-nonce": "0123456789abcdef",
      "x-xmcl-timestamp": "1000",
    },
    bodyBase64: Buffer.from('{"requestId":"request_1"}').toString("base64"),
  };
  assert.equal(
    new TextDecoder().decode(parseAcaDelivery(encoder.encode(JSON.stringify(valid))).body),
    '{"requestId":"request_1"}',
  );
  assert.throws(() => parseAcaDelivery(encoder.encode(JSON.stringify({
    ...valid,
    callbackUrl: "https://attacker.invalid",
  }))), /invalid_delivery/);
  assert.throws(() => decodeBase64("Zg", 10), /invalid base64/);
  const oversizedBody = Buffer.from(JSON.stringify({
    requestId: "request_1",
    padding: "a".repeat(60 * 1024),
  }));
  assert.throws(() => parseAcaDelivery(encoder.encode(JSON.stringify({
    ...valid,
    bodyBase64: oversizedBody.toString("base64"),
  }))), /invalid_delivery/);
});

test("one-shot worker refuses background mode and processes exactly one claim", async () => {
  const calls = [];
  const context = {
    started: false,
    async initialize() {
      calls.push("initialize");
    },
    queue: {
      async claim() {
        calls.push(["claim", ...arguments]);
        return { id: "one" };
      },
    },
    async processQueued(queued) {
      calls.push(queued.id);
      return { terminal: true, result: { status: "published" } };
    },
  };
  const result = await CompilerHttpWorker.prototype.runOne.call(context, "one");
  assert.deepEqual(calls, ["initialize", ["claim", "one"], "one"]);
  assert.equal(result.terminal, true);
  await assert.rejects(() => CompilerHttpWorker.prototype.runOne.call({
    ...context,
    started: true,
  }), /background consumers/);
});

test("ACA execution lease serializes the same compiler request and expires closed", async () => {
  const root = await mkdtemp(join(tmpdir(), "xmcl-aca-lease-"));
  try {
    const first = await acquireExecutionLease("request_1", {
      root,
      now: () => 1_000,
      leaseMs: 10_000,
    });
    await assert.rejects(() => acquireExecutionLease("request_1", {
      root,
      now: () => 2_000,
      leaseMs: 10_000,
    }), (error) => error?.code === "execution_already_active");
    await first.release();
    const second = await acquireExecutionLease("request_1", {
      root,
      now: () => 2_000,
      leaseMs: 10_000,
    });
    await second.release();
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("ACA outer sandbox is read-only except exact state and workspace paths", () => {
  const args = outerSandboxArguments();
  assert.ok(args.includes("--unshare-user"));
  assert.ok(args.includes("--unshare-pid"));
  assert.equal(args.includes("--unshare-net"), false);
  assert.deepEqual(bindSources(args, "--bind"), [
    "/run/xmcl-compiler/workspaces",
    "/var/lib/xmcl-compiler/replay",
    "/var/lib/xmcl-compiler/jobs",
    "/var/lib/xmcl-compiler/leases",
  ]);
  assert.ok(bindSources(args, "--ro-bind").includes("/opt/xmcl/jres"));
  assert.ok(bindSources(args, "--ro-bind").includes("/run/xmcl-compiler/config"));
});

test("Bubblewrap container probe preserves restrictive Docker defaults", () => {
  const run = dockerRunArguments();
  assert.ok(run.includes("--read-only"));
  assert.deepEqual(option(run, "--user"), ["10001:10001"]);
  assert.deepEqual(option(run, "--cap-drop"), ["ALL"]);
  assert.deepEqual(option(run, "--security-opt"), ["no-new-privileges"]);
  assert.equal(run.includes("--privileged"), false);
  assert.equal(run.includes("seccomp=unconfined"), false);
  const probe = bubblewrapProbeArguments({
    workspace: "/tmp/work",
    parentNetworkNamespace: "net:[1]",
  });
  for (const namespace of [
    "--unshare-user", "--unshare-pid", "--unshare-net", "--unshare-ipc", "--unshare-uts",
  ]) {
    assert.ok(probe.includes(namespace));
  }
});

test("ACA packaging defaults to canary and immutable reviewed JRE", async () => {
  const root = resolve(".");
  const [dockerfile, canaryBicep, moduleBicep,
    registry, worker, catalog, workflow] = await Promise.all([
    readFile(join(root, "Dockerfile.aca"), "utf8"),
    readFile(join(root, "deploy", "aca-job.bicep"), "utf8"),
    readFile(join(root, "deploy", "_aca-job.bicep"), "utf8"),
    readFile(join(root, "deploy", "jre-registry.aca.json"), "utf8"),
    readFile(join(root, "deploy", "worker.aca.example.json"), "utf8"),
    readFile(join(root, "toolchain-catalog.lock.json"), "utf8"),
    readFile(join(root, "..", ".github", "workflows", "publish-compiler-image.yml"), "utf8"),
  ]);
  assert.match(dockerfile, /^USER 10001:10001$/m);
  assert.match(dockerfile, /materialize_verified_jre\.mjs/);
  assert.match(dockerfile, /^CMD \["probe"\]$/m);
  assert.match(canaryBicep, /module compilerJob '.\/_aca-job\.bicep'/);
  assert.match(moduleBicep, /args:\s*\[\s*'probe'\s*\]/);
  assert.match(moduleBicep, /secrets: \[\]/);
  assert.match(moduleBicep, /env: \[\]/);
  assert.doesNotMatch(canaryBicep + moduleBicep,
    /entrypointMode|canaryApproved|workerConfigBase64|serviceHmacSecretBase64|'job'/);
  assert.match(moduleBicep,
    /ghcr\.io\/voxelum\/xmcl-shared-node-compiler@sha256:\$\{compilerImageDigest\}/);
  assert.match(moduleBicep, /replicaRetryLimit: 0/);
  assert.match(moduleBicep, /parallelism: 1/);
  assert.match(moduleBicep, /identity:\s*\{\s*type: 'None'/);
  assert.match(moduleBicep,
    /mountOptions: 'uid=10001,gid=10001,dir_mode=0700,file_mode=0600'/);
  assert.doesNotMatch(moduleBicep, /mountOptions: '[^']*(?:nosuid|nodev|noexec)/);
  assert.doesNotMatch(moduleBicep, /latest/);
  assert.equal(JSON.parse(registry).roots[0].path,
    "/opt/xmcl/jres/java-runtime-delta-21");
  assert.equal(JSON.parse(registry).catalogRevision, JSON.parse(catalog).catalogRevision);
  assert.equal(JSON.parse(worker).identity.secretPath,
    "/run/xmcl-compiler/config/service-hmac-secret");
  assert.match(workflow, /file: compiler\/Dockerfile\.aca/);
  const build = dockerBuildArguments();
  assert.equal(build[0], "build");
  assert.equal(build[1], "--file");
  assert.equal(build[2], join(resolve(".."), "compiler", "Dockerfile.aca"));
  assert.equal(build.at(-1), resolve(".."));
});

function bindSources(args, flag) {
  const values = [];
  for (let index = 0; index < args.length; index += 1) {
    if (args[index] === flag) values.push(args[index + 1]);
  }
  return values;
}

function option(args, name) {
  const index = args.indexOf(name);
  return index < 0 ? [] : [args[index + 1]];
}
