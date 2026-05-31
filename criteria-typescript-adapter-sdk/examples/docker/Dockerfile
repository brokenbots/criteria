# Dockerfile for remote criteria adapter (TypeScript)
# Build from the repository root so the proto/ tree is in context:
#   docker build -f criteria-typescript-adapter-sdk/examples/docker/Dockerfile -t adapter .
FROM oven/bun:1-alpine AS builder

WORKDIR /app
COPY criteria-typescript-adapter-sdk/package.json criteria-typescript-adapter-sdk/bun.lock criteria-typescript-adapter-sdk/tsconfig.json ./
COPY criteria-typescript-adapter-sdk/src ./src
COPY proto ./proto
RUN bun install --frozen-lockfile && bun run build

FROM oven/bun:1-alpine
WORKDIR /app
COPY --from=builder /app/dist ./dist
COPY --from=builder /app/node_modules ./node_modules
COPY criteria-typescript-adapter-sdk/package.json ./
COPY proto ./proto

# The adapter runs as a remote adapter; it dials out to the criteria host.
ENV NODE_ENV=production
ENV CRITERIA_REMOTE_TOKEN=""

CMD ["bun", "run", "dist/index.js"]
