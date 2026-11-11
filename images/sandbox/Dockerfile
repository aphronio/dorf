FROM node:26-trixie-slim@sha256:5adad64f9a1fca2d51972caa65983634c8a774eb300d72b20f7f2a0f60368072
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates chromium gh git python3 tini \
 && rm -rf /var/lib/apt/lists/* && mkdir /workspace
# Root in a Sandbox lacks setuid and DAC override, so apt must not hand its dirs to _apt.
RUN echo 'APT::Sandbox::User "root";' > /etc/apt/apt.conf.d/90sandbox \
 && chown -R root:root /var/cache/apt /var/lib/apt
# A placeholder the egress gateway swaps for the real token, so the Sandbox never holds it.
RUN git config --system credential.https://github.com.helper \
  '!f() { echo username=x-access-token; echo password=placeholder; }; f' \
 && git config --system user.name Dorf \
 && git config --system user.email dorf@users.noreply.github.com
COPY --from=pienv pi-env /usr/local/bin/pi-env
# Browser Harness drives the headless Chromium over CDP; its skill sits where a Thread's skills section finds it.
RUN --mount=from=ghcr.io/astral-sh/uv:0.13.7@sha256:96f40d3904aad8d0bd010539001d36c8e2188b981e1334471a90c4a1d93bb490,source=/uv,target=/usr/local/bin/uv \
    UV_PYTHON_DOWNLOADS=never uv venv -q --python /usr/bin/python3 /opt/browser-harness \
 && VIRTUAL_ENV=/opt/browser-harness uv pip install -q --no-cache browser-harness==0.1.13 \
 && mkdir -p /.agents/skills/browser-harness \
 && /opt/browser-harness/bin/browser-harness skill > /.agents/skills/browser-harness/SKILL.md
COPY browser-harness /usr/local/bin/browser-harness
COPY --from=oven/bun:1.4.4-slim@sha256:1846adf22496bd711036aae8be147e57bdebbaea2e5264e3e0b5af1db187d9a4 /usr/local/bin/bun /usr/local/bin/bun
COPY package.json bun.lock /init/
RUN cd /init && bun install --frozen-lockfile --ignore-scripts --production
COPY init.mts /init/init.mts
# gh needs a token to send; the egress gateway replaces it, so the Sandbox never holds the real one.
ENV GH_TOKEN=placeholder \
    SSL_CERT_FILE=/run/ate/egress-trust-bundle/trust-bundle.pem \
    NODE_EXTRA_CA_CERTS=/run/ate/egress-trust-bundle/trust-bundle.pem \
    GIT_SSL_CAINFO=/run/ate/egress-trust-bundle/trust-bundle.pem \
    REQUESTS_CA_BUNDLE=/run/ate/egress-trust-bundle/trust-bundle.pem \
    BU_CDP_URL=http://127.0.0.1:9222
WORKDIR /workspace
# tini reaps orphans, which init.mts as PID 1 would leave as zombies.
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["node", "/init/init.mts"]
