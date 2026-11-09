FROM node:26-trixie-slim@sha256:5adad64f9a1fca2d51972caa65983634c8a774eb300d72b20f7f2a0f60368072
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git \
 && rm -rf /var/lib/apt/lists/* && mkdir /workspace
# Root in a Sandbox lacks setuid and DAC override, so apt must not hand its dirs to _apt.
RUN echo 'APT::Sandbox::User "root";' > /etc/apt/apt.conf.d/90sandbox \
 && chown -R root:root /var/cache/apt /var/lib/apt
# The daemon comes from the agent's pinned pi-env package, so client and daemon always match.
COPY --from=pienv pi-env /usr/local/bin/pi-env
COPY init.mts /init.mts
WORKDIR /workspace
CMD ["node", "/init.mts"]
