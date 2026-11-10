FROM node:26-trixie-slim@sha256:5adad64f9a1fca2d51972caa65983634c8a774eb300d72b20f7f2a0f60368072
WORKDIR /gate
COPY package.json package-lock.json ./
RUN npm ci --ignore-scripts --omit=dev --no-audit --no-fund
ARG SUBSTRATE_VERSION
ADD --chmod=644 https://raw.githubusercontent.com/agent-substrate/substrate/${SUBSTRATE_VERSION}/api/ateapi/v1/ateapi.proto ./
COPY ateapi.ts gate.ts ./
USER node
CMD ["node", "gate.ts"]
