FROM node:22-bookworm-slim@sha256:83f487e0a63425e5b4d146fb5e5be574bcbe1b7b843d3ebafdd95eaf7767a7e5

WORKDIR /opt/xmcl-compiler/app
RUN apt-get update \
    && apt-get install --no-install-recommends -y bubblewrap ca-certificates util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /run/xmcl-compiler/workspaces /var/lib/xmcl-compiler/replay \
    && chown -R 10001:10001 /run/xmcl-compiler /var/lib/xmcl-compiler \
    && chmod 0700 /run/xmcl-compiler/workspaces /var/lib/xmcl-compiler/replay
COPY --chown=10001:10001 package.json ./
COPY --chown=10001:10001 src ./src
COPY --chown=10001:10001 toolchain-catalog.lock.json ./
USER 10001:10001
ENV HOME=/nonexistent \
    NODE_ENV=production \
    HOST=0.0.0.0 \
    PORT=8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["node", "-e", "fetch('http://127.0.0.1:8080/healthz').then((r) => process.exit(r.ok ? 0 : 1)).catch(() => process.exit(1))"]
CMD ["node", "src/worker-server.mjs"]
