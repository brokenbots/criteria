# Dockerfile for remote criteria adapter (TypeScript)
FROM oven/bun:1-alpine AS builder

WORKDIR /app
COPY package.json bun.lockb tsconfig.json ./
COPY src ./src
RUN bun install --frozen-lockfile && bun run build

FROM oven/bun:1-alpine
WORKDIR /app
COPY --from=builder /app/dist ./dist
COPY --from=builder /app/node_modules ./node_modules
COPY package.json ./

# The adapter runs as a remote adapter; it dials out to the criteria host.
ENV NODE_ENV=production
ENV CRITERIA_REMOTE_TOKEN=""

CMD ["bun", "run", "dist/index.js"]
