# Multi-stage build: build the static site with Vite, then serve dist/ with
# an unprivileged nginx. It runs as uid 101 and listens on 8080, the port
# the Platform routes a Static site to when its values say runAsNonRoot
# (iidp app create writes that for this template); the Platform sets no PORT
# for it.

ARG NODE_VERSION=24-slim

FROM node:${NODE_VERSION} AS build
WORKDIR /app
COPY package.json ./
RUN npm install
COPY . .
RUN npm run build

# nginx's unprivileged image on the stable branch. Even minor versions are
# stable, odd ones mainline, and only the current stable and mainline
# branches are rebuilt with fixes, so move this to the next even branch when
# nginx releases one. Keep the unprivileged image: the Platform runs this
# container as non-root with every capability dropped, where the official
# nginx image, which starts as root on port 80, cannot start.
FROM nginxinc/nginx-unprivileged:1.30-alpine
COPY --from=build /app/dist /usr/share/nginx/html

EXPOSE 8080
