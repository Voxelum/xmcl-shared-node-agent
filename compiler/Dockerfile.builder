FROM node:22-bookworm-slim@sha256:83f487e0a63425e5b4d146fb5e5be574bcbe1b7b843d3ebafdd95eaf7767a7e5 AS jre-builder

WORKDIR /build
COPY compiler/package.json ./compiler/
COPY compiler/src/build-spec.mjs \
     compiler/src/bundle.mjs \
     compiler/src/production-limits.mjs \
     compiler/src/reviewed-builder.mjs \
     compiler/src/toolchain-catalog.mjs ./compiler/src/
COPY compiler/scripts/materialize_verified_jre.mjs ./compiler/scripts/
COPY compiler/toolchain-catalog.lock.json ./compiler/
COPY runtime-image/runtime-catalog.lock.json ./runtime-image/
RUN node compiler/scripts/materialize_verified_jre.mjs \
      --runtime-catalog-lock /build/runtime-image/runtime-catalog.lock.json \
      --toolchain-catalog-lock /build/compiler/toolchain-catalog.lock.json \
      --jre-id java-runtime-delta-21 \
      --output-root /build/jres/java-runtime-delta-21 \
      --registry-output /build/jre-registry.json \
      --registry-root /opt/xmcl/jres/java-runtime-delta-21

FROM node:22-bookworm-slim@sha256:83f487e0a63425e5b4d146fb5e5be574bcbe1b7b843d3ebafdd95eaf7767a7e5

ARG SOURCE_REVISION
ARG BUILDER_SCHEMA=agent_builder_v1
ARG TOOLCHAIN_CATALOG_REVISION
ARG RUNTIME_CATALOG_REVISION
LABEL org.opencontainers.image.revision="${SOURCE_REVISION}" \
      io.xmcl.builder.schema="${BUILDER_SCHEMA}" \
      io.xmcl.builder.toolchain-catalog-revision="${TOOLCHAIN_CATALOG_REVISION}" \
      io.xmcl.builder.runtime-catalog-revision="${RUNTIME_CATALOG_REVISION}" \
      io.xmcl.builder.target="linux-amd64"

RUN apt-get update \
    && apt-get install --no-install-recommends -y util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /opt/xmcl-builder/app /opt/xmcl-builder/config /opt/xmcl/jres /input /output \
    && chown 10001:10001 /output \
    && chmod 0700 /output

WORKDIR /opt/xmcl-builder/app
COPY --chown=10001:10001 compiler/package.json ./
COPY --chown=10001:10001 compiler/src/build-spec.mjs \
     compiler/src/bundle.mjs \
     compiler/src/production-limits.mjs \
     compiler/src/reviewed-builder.mjs \
     compiler/src/toolchain-catalog.mjs \
     compiler/src/agent-builder.mjs \
     compiler/src/bubblewrap-sandbox.mjs \
     compiler/src/outer-docker-sandbox.mjs \
     compiler/src/process-quiescer.mjs \
     compiler/src/verified-jre-registry.mjs ./src/
COPY --chown=10001:10001 compiler/scripts/agent_builder_v1.mjs ./scripts/
COPY --chown=10001:10001 compiler/toolchain-catalog.lock.json ./
COPY --from=jre-builder /build/jre-registry.json /opt/xmcl-builder/config/jre-registry.json
COPY --from=jre-builder /build/jres/java-runtime-delta-21 /opt/xmcl/jres/java-runtime-delta-21

RUN test "$BUILDER_SCHEMA" = agent_builder_v1 \
    && node -e "const c=require('./toolchain-catalog.lock.json');if(c.catalogRevision!==process.argv[1]||c.runtimeCatalogRevision!==process.argv[2])process.exit(1)" "$TOOLCHAIN_CATALOG_REVISION" "$RUNTIME_CATALOG_REVISION" \
    && chmod -R a-w /opt/xmcl-builder /opt/xmcl/jres \
    && chmod 0555 /opt/xmcl-builder/app/scripts/agent_builder_v1.mjs /opt/xmcl/jres/java-runtime-delta-21/bin/java

USER 10001:10001
ENTRYPOINT ["/usr/local/bin/node", "/opt/xmcl-builder/app/scripts/agent_builder_v1.mjs"]
