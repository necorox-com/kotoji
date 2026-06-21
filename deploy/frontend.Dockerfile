# Frontend image: Next.js 16 standalone. Multi-stage — install deps, build the
# standalone server, then ship a minimal Node runtime with only the standalone
# output. Runs as a non-root user.
#
# Build context is the frontend/ directory (see docker-compose.yml `context: ../frontend`).

# ---- deps stage ----
FROM node:22-alpine AS deps
WORKDIR /app
# Install from the lockfile for reproducible builds.
COPY package.json package-lock.json ./
RUN npm ci

# ---- build stage ----
FROM node:22-alpine AS build
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
# Telemetry off in CI/containers; build the standalone bundle (next.config.ts has
# output: "standalone").
ENV NEXT_TELEMETRY_DISABLED=1
RUN npm run build

# ---- runtime stage ----
FROM node:22-alpine AS runtime
WORKDIR /app
ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1
ENV PORT=3000

# Run as the built-in non-root `node` user.
RUN mkdir -p /app && chown -R node:node /app

# The standalone output contains a minimal node_modules + server.js. Static and
# public assets are copied alongside per Next's standalone layout.
COPY --from=build --chown=node:node /app/.next/standalone ./
COPY --from=build --chown=node:node /app/.next/static ./.next/static
COPY --from=build --chown=node:node /app/public ./public

USER node
EXPOSE 3000

# server.js is the standalone entrypoint Next emits.
CMD ["node", "server.js"]
