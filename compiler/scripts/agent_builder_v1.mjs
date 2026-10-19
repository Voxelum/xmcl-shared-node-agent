#!/usr/bin/env node
import { pathToFileURL } from "node:url";
import { isAbsolute } from "node:path";
import { CompilerFailure } from "../src/bundle.mjs";
import {
  AGENT_BUILDER_PATHS,
  runAgentBuilderV1,
} from "../src/agent-builder.mjs";

export async function main(argv = process.argv.slice(2)) {
  await runAgentBuilderV1({ paths: builderPathsFromArgs(argv) });
}

export function builderPathsFromArgs(argv = []) {
  if (!Array.isArray(argv) || argv.length % 2 !== 0) {
    throw new CompilerFailure("invalid_builder_invocation");
  }
  const paths = { ...AGENT_BUILDER_PATHS };
  const options = new Map([
    ["--build-spec", "buildSpec"],
    ["--source", "source"],
    ["--dependencies-index", "dependencyIndex"],
    ["--dependencies", "dependencies"],
    ["--output", "output"],
  ]);
  const seen = new Set();
  for (let index = 0; index < argv.length; index += 2) {
    const field = options.get(argv[index]);
    const value = argv[index + 1];
    if (!field || seen.has(field) || typeof value !== "string" ||
      !isAbsolute(value) || value.length > 1_024) {
      throw new CompilerFailure("invalid_builder_invocation");
    }
    seen.add(field);
    paths[field] = value;
  }
  return Object.freeze(paths);
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((error) => {
    const code = error instanceof CompilerFailure &&
      /^[a-z][a-z0-9_]{0,63}$/.test(error.code)
      ? error.code
      : "builder_failed";
    process.stderr.write(`agent_builder_v1 failed: ${code}\n`);
    process.exitCode = 1;
  });
}
