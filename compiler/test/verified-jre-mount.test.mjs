import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import vm from "node:vm";
import test from "node:test";

// Exercise the actual private parser without widening the production API or
// depending on the test host's own /proc mount layout.
const source = await readFile(new URL("../src/verified-jre-registry.mjs", import.meta.url), "utf8");
const parser = source.slice(source.indexOf("async function readOnlyMount("), source.indexOf("function within("));
const mount = (point, flags, id = 36) => `${id} 25 0:32 / ${point} ${flags},relatime - overlay overlay rw`;
for (const [name, lines, expected] of [
  ["read-only root covers descendants", [mount("/", "ro")], true],
  ["writable root covers descendants", [mount("/", "rw")], false],
  ["nested writable mount overrides read-only root", [mount("/", "ro"), mount("/opt", "rw", 37)], false],
  ["read-only child overrides writable parent regardless of line order", [mount("/opt/xmcl", "ro", 38), mount("/", "ro"), mount("/opt", "rw", 37)], true],
  ["sibling prefix does not match", [mount("/", "rw"), mount("/opt/xmc", "ro", 37)], false],
]) {
  test(name, async () => {
    const context = vm.createContext({ readFile: async () => lines.join("\n") });
    const actual = await vm.runInContext(`${parser}\nreadOnlyMount('/opt/xmcl/jres/java-runtime-delta-21')`, context);
    assert.equal(actual, expected);
  });
}
