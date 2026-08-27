#!/usr/bin/env node
import { readFile, writeFile } from "node:fs/promises";
import { resolve } from "node:path";
import {
  formatCatalog,
  generateToolchainCatalog,
  ToolchainCatalogFailure,
} from "../src/toolchain-catalog.mjs";

const root = resolve(import.meta.dirname, "..");

async function main() {
  const { runtimeCatalogLock, output, check } = parseArguments(process.argv.slice(2));
  const runtimeCatalogBytes = new Uint8Array(await readFile(runtimeCatalogLock));
  const catalog = await generateToolchainCatalog({ runtimeCatalogBytes });
  const next = formatCatalog(catalog);
  let previous = "";
  try {
    previous = await readFile(output, "utf8");
  } catch (error) {
    if (error?.code !== "ENOENT") throw error;
  }
  if (previous === next) {
    process.stdout.write(`toolchain catalog unchanged: ${output}\n`);
    return;
  }
  if (check) {
    throw new ToolchainCatalogFailure("catalog_update_required");
  }
  await writeFile(output, next, "utf8");
  process.stdout.write(`toolchain catalog updated: ${output}\n`);
}

function parseArguments(arguments_) {
  let runtimeCatalogLock;
  let output = resolve(root, "toolchain-catalog.lock.json");
  let check = false;
  for (let index = 0; index < arguments_.length; index += 1) {
    const argument = arguments_[index];
    if (argument === "--runtime-catalog-lock") {
      runtimeCatalogLock = resolveRequired(arguments_[++index], argument);
    } else if (argument === "--output") {
      output = resolveRequired(arguments_[++index], argument);
    } else if (argument === "--check") {
      check = true;
    } else {
      throw new ToolchainCatalogFailure("invalid_arguments");
    }
  }
  if (!runtimeCatalogLock) throw new ToolchainCatalogFailure("runtime_catalog_lock_required");
  return { runtimeCatalogLock, output, check };
}

function resolveRequired(value, argument) {
  if (!value || value.startsWith("-")) throw new ToolchainCatalogFailure(`missing_${argument.slice(2)}`);
  return resolve(value);
}

main().catch((error) => {
  const target = error.url ? ` (${error.url})` : "";
  process.stderr.write(`toolchain catalog update failed: ${error.code ?? error.message}${target}\n`);
  process.exitCode = 1;
});
