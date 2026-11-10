FROM node:26-trixie-slim@sha256:5adad64f9a1fca2d51972caa65983634c8a774eb300d72b20f7f2a0f60368072
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git tini \
 && rm -rf /var/lib/apt/lists/* && mkdir /workspace
# Root in a Sandbox lacks setuid and DAC override, so apt must not hand its dirs to _apt.
RUN echo 'APT::Sandbox::User "root";' > /etc/apt/apt.conf.d/90sandbox \
 && chown -R root:root /var/cache/apt /var/lib/apt
# Git answers GitHub's 401 with a placeholder; the egress gateway swaps in the real token, so
# the Sandbox never holds it. Public clones get no 401 and send nothing.
RUN git config --system credential.https://github.com.helper \
  '!f() { echo username=x-access-token; echo password=placeholder; }; f'
# The daemon comes from the agent's pinned pi-env package, so client and daemon always match.
COPY --from=pienv pi-env /usr/local/bin/pi-env
COPY package.json package-lock.json /init/
RUN cd /init && npm ci --ignore-scripts --omit=dev --no-audit --no-fund
COPY init.mts /init/init.mts
# The template mounts Substrate's trust bundle here: public roots plus the egress gateway's CA,
# which signs the hosts whose requests it rewrites, such as credential injection.
ENV SSL_CERT_FILE=/run/ate/egress-trust-bundle/trust-bundle.pem \
    NODE_EXTRA_CA_CERTS=/run/ate/egress-trust-bundle/trust-bundle.pem \
    GIT_SSL_CAINFO=/run/ate/egress-trust-bundle/trust-bundle.pem \
    REQUESTS_CA_BUNDLE=/run/ate/egress-trust-bundle/trust-bundle.pem
WORKDIR /workspace
# tini reaps orphans, which init.mts as PID 1 would leave as zombies.
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["node", "/init/init.mts"]
