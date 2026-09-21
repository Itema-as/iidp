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

RUN addgroup --system --gid 1001 nodejs \
  && adduser --system --uid 1001 nextjs

COPY --from=builder /app/public ./public
COPY --from=builder --chown=nextjs:nodejs /app/.next/standalone ./
COPY --from=builder --chown=nextjs:nodejs /app/.next/static ./.next/static

USER nextjs

# The Platform injects PORT (default 3000); server.js reads it.
EXPOSE 3000

CMD ["node", "server.js"]
