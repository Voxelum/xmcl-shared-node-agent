import { createCompilerHttpServer } from "./http-worker.mjs";

const port = parsePort(process.env.PORT);
const host = parseHost(process.env.HOST);
const server = createCompilerHttpServer();

server.listen(port, host, () => {
  process.stdout.write(`xmcl compiler worker listening on ${host}:${port}\n`);
});

for (const signal of ["SIGINT", "SIGTERM"]) {
  process.once(signal, () => {
    server.close(() => process.exit(0));
    setTimeout(() => process.exit(1), 10_000).unref();
  });
}

function parsePort(value) {
  return typeof value === "string" && /^(?:[1-9]\d{0,4})$/.test(value) &&
    Number(value) <= 65_535 ? Number(value) : 8080;
}

function parseHost(value) {
  return ["0.0.0.0", "127.0.0.1", "::"].includes(value) ? value : "0.0.0.0";
}
