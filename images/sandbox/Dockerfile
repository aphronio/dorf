FROM node:26-trixie-slim@sha256:5adad64f9a1fca2d51972caa65983634c8a774eb300d72b20f7f2a0f60368072
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git \
 && rm -rf /var/lib/apt/lists/* && mkdir /workspace
# The daemon comes from the agent's pinned pi-env package, so client and daemon always match.
COPY --from=pienv pi-env /usr/local/bin/pi-env
COPY init.mts /init.mts
WORKDIR /workspace
CMD ["node", "/init.mts"]
