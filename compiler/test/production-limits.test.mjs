import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import test from "node:test";
import {
  PRODUCTION_OUTPUT_LIMITS,
  outputSizeWithinLimit,
} from "../src/production-limits.mjs";
import { MAX_BUNDLE_LOGICAL_BYTES } from "../src/bundle.mjs";
import { PRODUCTION_PATHS } from "../src/production-composition.mjs";

test("production output budgets enforce exact memory-derived boundaries", () => {
  const limits = PRODUCTION_OUTPUT_LIMITS;
  assert.equal(outputSizeWithinLimit(
    [limits.maxFileBytes, limits.maxSandboxBytes - limits.maxFileBytes],
    limits.maxSandboxBytes,
  ), true);
  assert.equal(outputSizeWithinLimit(
    [limits.maxFileBytes, limits.maxSandboxBytes - limits.maxFileBytes + 1],
    limits.maxSandboxBytes,
  ), false);
  assert.equal(outputSizeWithinLimit([
    limits.maxFileBytes,
    limits.maxPackageInputBytes - limits.maxFileBytes,
  ], limits.maxPackageInputBytes), true);
  assert.equal(outputSizeWithinLimit(
    [limits.maxFileBytes, limits.maxPackageInputBytes - limits.maxFileBytes, 1],
    limits.maxPackageInputBytes,
  ), false);
  assert.equal(outputSizeWithinLimit([limits.maxFileBytes + 1],
    limits.maxPackageInputBytes), false);
  assert.ok(limits.maxSandboxBytes < limits.maxPackageInputBytes);
  assert.ok(limits.maxPackageInputBytes < limits.maxBundleArchiveBytes);
  assert.ok(limits.maxPackageInputBytes < limits.maxPackageTarBytes);
  assert.ok(limits.maxPackageTarBytes < limits.maxPackageArchiveBytes);
  assert.equal(MAX_BUNDLE_LOGICAL_BYTES, limits.maxPackageInputBytes);
});

test("supported staging unit and example config are fail-closed", async () => {
  const service = await readFile(resolve("deploy/xmcl-compiler.service"), "utf8");
  const config = JSON.parse(await readFile(resolve("worker.example.json"), "utf8"));
  for (const directive of [
    "User=10001",
    "Group=10001",
    "NoNewPrivileges=yes",
    "CapabilityBoundingSet=",
    "ProtectSystem=strict",
    "PrivateDevices=yes",
    "MemoryMax=3200M",
    "MemorySwapMax=0",
    "CPUQuota=200%",
    "LoadCredential=service-hmac-secret:",
  ]) {
    assert.ok(service.includes(directive), `missing ${directive}`);
  }
  assert.ok(!service.includes("User=root"));
  assert.equal(config.sandbox.fileSizeBytes, PRODUCTION_OUTPUT_LIMITS.maxFileBytes);
  assert.equal(config.maxConcurrentJobs, 1);
  assert.equal(PRODUCTION_PATHS.catalog,
    "/opt/xmcl-compiler/app/toolchain-catalog.lock.json");
  assert.equal(PRODUCTION_PATHS.workspaceRoot, "/run/xmcl-compiler/workspaces");
  assert.equal(PRODUCTION_PATHS.secretsRoot,
    "/run/credentials/xmcl-compiler.service");
});
