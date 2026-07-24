# The release pipeline supplies a verified static launcher and unpacked JRE
# assets in deploy/runtime/runtime-assets. This Dockerfile has no downloader,
# package manager, or mutable base image.
FROM scratch
COPY deploy/runtime/runtime-assets/busybox /bin/sh
COPY deploy/runtime/runtime-assets/jre8 /opt/xmcl/jre/8
COPY deploy/runtime/runtime-assets/jre17 /opt/xmcl/jre/17
COPY deploy/runtime/runtime-assets/jre21 /opt/xmcl/jre/21
COPY deploy/runtime/runtime-assets/xmcl-shared-minecraft-runtime /usr/local/bin/xmcl-shared-minecraft-runtime
USER 1000:1000
WORKDIR /data
VOLUME ["/data"]
EXPOSE 25565/tcp
HEALTHCHECK --interval=30s --timeout=5s --start-period=90s --retries=3 CMD ["/usr/local/bin/xmcl-shared-minecraft-runtime", "health"]
ENTRYPOINT ["/usr/local/bin/xmcl-shared-minecraft-runtime"]
