FROM node:26-trixie-slim@sha256:5adad64f9a1fca2d51972caa65983634c8a774eb300d72b20f7f2a0f60368072
WORKDIR /keys
COPY package.json bun.lock ./
RUN --mount=from=oven/bun:1.4.4-slim@sha256:1846adf22496bd711036aae8be147e57bdebbaea2e5264e3e0b5af1db187d9a4,source=/usr/local/bin/bun,target=/usr/local/bin/bun bun install --frozen-lockfile --ignore-scripts --production
COPY keys.ts ./
USER node
CMD ["node", "keys.ts"]
