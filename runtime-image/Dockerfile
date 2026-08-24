# This image is intentionally built only by the verified release workflow.
# runtime-assets/ is an ignored, checksum-validated build input containing
# the support assets and the Java directories materialized from the catalog lock.
FROM gcr.io/distroless/java-base-debian12@sha256:e5ceff75002cf4f0b5f99e0a1b614757f8e913103dac26015684024f635f7674

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
