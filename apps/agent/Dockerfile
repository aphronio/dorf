FROM node:26-trixie-slim@sha256:5adad64f9a1fca2d51972caa65983634c8a774eb300d72b20f7f2a0f60368072 AS web
COPY --from=oven/bun:1.4.4-slim@sha256:1846adf22496bd711036aae8be147e57bdebbaea2e5264e3e0b5af1db187d9a4 /usr/local/bin/bun /usr/local/bin/bun
WORKDIR /web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile --ignore-scripts
COPY web ./
RUN NEXT_TELEMETRY_DISABLED=1 bun run build

FROM node:26-trixie-slim@sha256:5adad64f9a1fca2d51972caa65983634c8a774eb300d72b20f7f2a0f60368072
WORKDIR /agent
COPY agent/package.json agent/bun.lock ./
RUN --mount=from=web,source=/usr/local/bin/bun,target=/usr/local/bin/bun bun install --frozen-lockfile --ignore-scripts --production
COPY agent/src ./src
COPY --from=web /web/out ./web
USER node
CMD ["node", "src/main.ts"]
