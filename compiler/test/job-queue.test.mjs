import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { readFile, readdir, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import test from "node:test";
import { FilesystemCompilerJobQueue } from "../src/job-queue.mjs";

const encoder = new TextEncoder();

test("filesystem queue persists exact jobs, recovers running work, deduplicates, and archives terminal jobs", async () => {
  const directory = join(process.cwd(), ".test-artifacts", `compiler-queue-${randomUUID()}`);
  let now = 1_000;
  const input = {
    id: "request_1",
    deploymentId: "deployment_1",
    jobFingerprint: "a".repeat(64),
    body: encoder.encode('{"exact":"job"}'),
  };
  try {
    const first = new FilesystemCompilerJobQueue({
      directory,
      now: () => now,
      retryDelayMs: 100,
    });
    await first.initialize();
    assert.deepEqual(await first.enqueue(input), { duplicate: false });
    assert.deepEqual(await first.enqueue(input), { duplicate: true });
    const claimed = await first.claim();
    assert.equal(claimed.id, input.id);
    assert.deepEqual(claimed.body, input.body);

    const restarted = new FilesystemCompilerJobQueue({
      directory,
      now: () => now,
      retryDelayMs: 100,
    });
    await restarted.initialize();
    const recovered = await restarted.claim();
    assert.equal(recovered.id, input.id);
    assert.equal(recovered.attempts, 2);
    await restarted.finish(input.id, {
      kind: "retry",
      result: { status: "published_callback_uncertain" },
    });
    assert.deepEqual(await restarted.counts(), { pending: 1, archived: 0 });
    assert.equal(await restarted.claim(), undefined);

    now += 200;
    assert.equal((await restarted.claim()).id, input.id);
    await restarted.finish(input.id, {
      kind: "terminal",
      result: { status: "published" },
    });
    assert.deepEqual(await restarted.counts(), { pending: 0, archived: 1 });
    assert.deepEqual(await readdir(join(directory, "pending")), []);
    const archived = await readdir(join(directory, "archive"));
    assert.equal(archived.length, 1);
    assert.equal(archived.some((entry) => entry.endsWith(".partial")), false);
    assert.equal(JSON.parse(await readFile(
      join(directory, "archive", archived[0]),
      "utf8",
    )).state, "terminal");
    const terminalRecord = JSON.parse(await readFile(
      join(directory, "archive", archived[0]),
      "utf8",
    ));
    await writeFile(
      join(directory, "pending", archived[0]),
      JSON.stringify({ ...terminalRecord, state: "running" }),
    );
    const afterArchiveCrash = new FilesystemCompilerJobQueue({
      directory,
      now: () => now,
      retryDelayMs: 100,
    });
    await afterArchiveCrash.initialize();
    assert.deepEqual(await afterArchiveCrash.counts(), { pending: 0, archived: 1 });
    assert.deepEqual(await afterArchiveCrash.enqueue(input), { duplicate: true });
    await assert.rejects(() => afterArchiveCrash.enqueue({
      ...input,
      deploymentId: "deployment_other",
    }), (error) => error?.code === "idempotency_conflict");
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
