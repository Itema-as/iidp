# Multi-stage build producing a small runtime image for a Next.js Web
# service. The container must listen on the port the Platform sets in the
# PORT environment variable; Next.js honours it in standalone output.

ARG NODE_VERSION=24-slim

FROM node:${NODE_VERSION} AS deps
WORKDIR /app
COPY package.json ./
RUN npm install

FROM node:${NODE_VERSION} AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN npm run build

FROM node:${NODE_VERSION} AS runner
WORKDIR /app
ENV NODE_ENV=production
ENV HOSTNAME=0.0.0.0

# A real, writable home: a --system user otherwise gets /nonexistent, where
# npm can't keep its cache or logs, and npx (which Prisma and similar tools
# use for migrations) fails.
RUN addgroup --system --gid 1001 nodejs \
  && adduser --system --uid 1001 --ingroup nodejs --home /home/nextjs nextjs
ENV HOME=/home/nextjs

COPY --from=builder /app/public ./public
COPY --from=builder --chown=nextjs:nodejs /app/.next/standalone ./
COPY --from=builder --chown=nextjs:nodejs /app/.next/static ./.next/static

# By number, not name: the Platform runs this image with runAsNonRoot,
# and the kubelet can only verify a numeric user is not root (a named one
# fails to start with "cannot verify user is non-root").
USER 1001:1001

# The Platform injects PORT (default 3000); server.js reads it.
EXPOSE 3000

CMD ["node", "server.js"]
