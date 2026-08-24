# This image is intentionally built only by the verified release workflow.
# runtime-assets/ is an ignored, checksum-validated build input containing
# the support assets and the Java directories materialized from the catalog lock.
FROM gcr.io/distroless/base-debian12@sha256:d2add786f2a5f43d1ab3ae54cd3193de929d0d12c378ff60891921f56f3e47ff

ARG RUNTIME_CATALOG_SHA256
LABEL io.xmcl.runtime-catalog-sha256="${RUNTIME_CATALOG_SHA256}"

COPY runtime-assets/busybox /bin/sh
COPY runtime-assets/jre /opt/xmcl/jre
COPY runtime-assets/xmcl-shared-minecraft-runtime /usr/local/bin/xmcl-shared-minecraft-runtime

USER 1000:1000
WORKDIR /data
VOLUME ["/data"]
EXPOSE 25565/tcp
HEALTHCHECK --interval=30s --timeout=5s --start-period=90s --retries=3 CMD ["/usr/local/bin/xmcl-shared-minecraft-runtime", "health"]
ENTRYPOINT ["/usr/local/bin/xmcl-shared-minecraft-runtime"]
