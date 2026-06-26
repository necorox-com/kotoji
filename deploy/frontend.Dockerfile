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
# Bind the Next.js standalone server to ALL interfaces. Docker auto-sets HOSTNAME
# to the container ID, and `next start`/standalone honours $HOSTNAME (server.js:
# `process.env.HOSTNAME || "0.0.0.0"`), which resolves to ONLY the first/default
# network IP. Behind the edge overlay the frontend is also attached to the `edge`
# network and Traefik routes to its edge-network IP — on which the server would
# NOT be listening, yielding 502s. Pinning 0.0.0.0 makes it accept on every
# attached network (no effect on the proxy-less base, where it is single-homed).
ENV HOSTNAME=0.0.0.0

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
