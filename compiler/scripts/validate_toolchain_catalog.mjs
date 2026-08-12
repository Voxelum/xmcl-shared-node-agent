#!/usr/bin/env node
import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import {
  formatCatalog,
  parseCatalogText,
  ToolchainCatalogFailure,
  validateToolchainCatalog,
} from "../src/toolchain-catalog.mjs";

const root = resolve(import.meta.dirname, "..");

async function main() {
  const { runtimeCatalogLock, catalog } = parseArguments(process.argv.slice(2));
  const [runtimeCatalogBytes, catalogText] = await Promise.all([
    readFile(runtimeCatalogLock),
    readFile(catalog, "utf8"),
  ]);
  const value = parseCatalogText(new TextEncoder().encode(catalogText));
  validateToolchainCatalog(value, new Uint8Array(runtimeCatalogBytes), {
    requireCanonical: true,
    rawLockText: catalogText,
  });
  if (catalogText !== formatCatalog(value)) {
    throw new ToolchainCatalogFailure("catalog_not_canonical");
  }
  process.stdout.write(`toolchain catalog valid: ${catalog}\n`);
}

function parseArguments(arguments_) {
  let runtimeCatalogLock;
  let catalog = resolve(root, "toolchain-catalog.lock.json");
  for (let index = 0; index < arguments_.length; index += 1) {
    const argument = arguments_[index];
    if (argument === "--runtime-catalog-lock") {
      runtimeCatalogLock = resolveRequired(arguments_[++index], argument);
    } else if (argument === "--catalog") {
      catalog = resolveRequired(arguments_[++index], argument);
    } else {
      throw new ToolchainCatalogFailure("invalid_arguments");
    }
  }
  if (!runtimeCatalogLock) throw new ToolchainCatalogFailure("runtime_catalog_lock_required");
  return { runtimeCatalogLock, catalog };
}

function resolveRequired(value, argument) {
  if (!value || value.startsWith("-")) throw new ToolchainCatalogFailure(`missing_${argument.slice(2)}`);
  return resolve(value);
}

main().catch((error) => {
  process.stderr.write(`toolchain catalog validation failed: ${error.code ?? error.message}\n`);
  process.exitCode = 1;
});
