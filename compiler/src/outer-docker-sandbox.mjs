import { constants } from "node:fs";
import {
  access,
  mkdir,
  mkdtemp,
  rm,
  writeFile,
} from "node:fs/promises";
import { resolve, sep } from "node:path";
import { CompilerFailure } from "./bundle.mjs";
import {
  collectFiles,
  mavenPath,
  rewriteNeoForgeInstaller,
  runProcess,
  selectNeoForgeArtifacts,
  validateRequest,
} from "./bubblewrap-sandbox.mjs";
import { PRODUCTION_OUTPUT_LIMITS } from "./production-limits.mjs";
import { quiesceContainerProcesses } from "./process-quiescer.mjs";

export class OuterDockerSandboxAdapter {
  constructor({
    jreRegistry,
    workspaceRoot,
    prlimitPath = "/usr/bin/prlimit",
    timeoutMs = 5 * 60 * 1000,
    addressSpaceBytes = 3 * 1024 * 1024 * 1024,
    fileSizeBytes = PRODUCTION_OUTPUT_LIMITS.maxFileBytes,
    processLimit = 256,
    cpuSeconds = 240,
    runner = runProcess,
    installerRewriter = rewriteNeoForgeInstaller,
    quiescer = quiesceContainerProcesses,
  } = {}) {
    if (!jreRegistry || typeof jreRegistry.rootForToken !== "function" ||
      !absoluteNormalizedPath(workspaceRoot) || !absoluteNormalizedPath(prlimitPath) ||
      typeof runner !== "function" ||
      !bounded(timeoutMs, 10_000, 15 * 60 * 1000) ||
      !bounded(addressSpaceBytes, 512 * 1024 * 1024, 8 * 1024 * 1024 * 1024) ||
      !bounded(fileSizeBytes, 64 * 1024 * 1024, PRODUCTION_OUTPUT_LIMITS.maxFileBytes) ||
      !bounded(processLimit, 16, 1_024) || !bounded(cpuSeconds, 10, 600) ||
      typeof installerRewriter !== "function" || typeof quiescer !== "function") {
      throw new TypeError("invalid outer Docker sandbox configuration");
    }
    this.jreRegistry = jreRegistry;
    this.workspaceRoot = workspaceRoot;
    this.prlimitPath = prlimitPath;
    this.timeoutMs = timeoutMs;
    this.addressSpaceBytes = addressSpaceBytes;
    this.fileSizeBytes = fileSizeBytes;
    this.processLimit = processLimit;
    this.cpuSeconds = cpuSeconds;
    this.runner = runner;
    this.installerRewriter = installerRewriter;
    this.quiescer = quiescer;
    this.capabilities = Object.freeze({
      ephemeralWorkspace: true,
      nonRoot: true,
      readOnlyBaseFilesystem: true,
      noSecrets: true,
      noDockerSocket: true,
      boundedResources: true,
      egress: "approved-artifacts-only",
    });
  }

  async initialize() {
    if (typeof process.getuid !== "function" || process.getuid() !== 10001 ||
      typeof process.getgid !== "function" || process.getgid() !== 10001) {
      throw new TypeError("sandbox worker must run as uid/gid 10001");
    }
    await Promise.all([
      access(this.prlimitPath, constants.X_OK),
      mkdir(this.workspaceRoot, { recursive: true, mode: 0o700 }),
    ]);
  }

  async assemble(request) {
    validateRequest(request);
    if (request.plan.family !== "neoforge") {
      throw new CompilerFailure("unsupported_compatibility");
    }
    const jreRoot = this.jreRegistry.rootForToken(request.jre.rootToken);
    await mkdir(this.workspaceRoot, { recursive: true, mode: 0o700 });
    const workspace = await mkdtemp(`${this.workspaceRoot}${sep}job-`);
    const inputs = `${workspace}${sep}inputs`;
    const output = `${workspace}${sep}output`;
    const temporary = `${workspace}${sep}tmp`;
    try {
      await Promise.all([
        mkdir(inputs, { mode: 0o700 }),
        mkdir(output, { mode: 0o700 }),
        mkdir(temporary, { mode: 0o700 }),
      ]);
      const { installer, server } = selectNeoForgeArtifacts(request);
      const offlineInstaller = this.installerRewriter(installer.bytes);
      await writeFile(`${inputs}${sep}installer.jar`, offlineInstaller, {
        mode: 0o400,
        flag: "wx",
      });
      const serverDirectory = `${output}${sep}libraries${sep}net${sep}minecraft${sep}server${sep}` +
        request.plan.minecraftVersion;
      await mkdir(serverDirectory, { recursive: true, mode: 0o700 });
      await writeFile(`${serverDirectory}${sep}server-${request.plan.minecraftVersion}.jar`,
        server.bytes, { mode: 0o600, flag: "wx" });
      for (const artifact of request.artifacts) {
        if (artifact === installer || artifact === server) continue;
        const relative = mavenPath(artifact);
        const target = `${output}${sep}libraries${sep}${relative.split("/").join(sep)}`;
        await mkdir(resolve(target, ".."), { recursive: true, mode: 0o700 });
        await writeFile(target, artifact.bytes, { mode: 0o600, flag: "wx" });
      }
      let runnerFailure;
      try {
        await this.runner(this.prlimitPath,
          this.#command(jreRoot, inputs, output, temporary), {
            timeoutMs: this.timeoutMs,
            cwd: output,
            env: Object.freeze({}),
          });
      } catch (error) {
        runnerFailure = error;
      }
      await this.quiescer();
      if (runnerFailure) throw runnerFailure;
      await Promise.all([
        "run.sh",
        "run.bat",
        "user_jvm_args.txt",
        "installer.log",
        "installer.jar.log",
      ].map((name) => rm(`${output}${sep}${name}`, { force: true })));
      const files = await collectFiles(output);
      return {
        files,
        attestation: {
          ephemeralWorkspace: true,
          nonRoot: true,
          readOnlyBaseFilesystem: true,
          noSecrets: true,
          noDockerSocket: true,
          boundedResources: true,
          network: "disabled",
        },
      };
    } catch (error) {
      if (error instanceof CompilerFailure) throw error;
      throw new CompilerFailure("installer_failed");
    } finally {
      await rm(workspace, { recursive: true, force: true }).catch(() => undefined);
    }
  }

  #command(jreRoot, inputs, output, temporary) {
    return [
      `--as=${this.addressSpaceBytes}`,
      `--fsize=${this.fileSizeBytes}`,
      `--nproc=${this.processLimit}`,
      `--cpu=${this.cpuSeconds}`,
      "--",
      `${jreRoot}${sep}bin${sep}java`,
      "-Djava.awt.headless=true",
      `-Duser.home=${output}`,
      `-Djava.io.tmpdir=${temporary}`,
      "-Xms128m",
      "-Xmx1024m",
      "-Xss512k",
      "-XX:MaxMetaspaceSize=512m",
      "-XX:CompressedClassSpaceSize=256m",
      "-XX:ReservedCodeCacheSize=128m",
      "-XX:MaxDirectMemorySize=256m",
      "-XX:ActiveProcessorCount=2",
      "-jar", `${inputs}${sep}installer.jar`,
      "--offline",
      "--installServer", output,
    ];
  }
}

function absoluteNormalizedPath(value) {
  return typeof value === "string" && value.length > 1 && value.length <= 1_024 &&
    resolve(value) === value;
}

function bounded(value, minimum, maximum) {
  return Number.isSafeInteger(value) && value >= minimum && value <= maximum;
}
