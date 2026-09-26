# Multi-stage build: build the static site with Vite, then serve dist/ with
# nginx. A Static site listens on 80; the Platform sets no PORT for it.

ARG NODE_VERSION=24-slim

FROM node:${NODE_VERSION} AS build
WORKDIR /app
COPY package.json ./
RUN npm install
COPY . .
RUN npm run build

# nginx's stable branch. Even minor versions are stable, odd ones mainline,
# and only the current stable and mainline branches are rebuilt with fixes,
# so move this to the next even branch when nginx releases one.
FROM nginx:1.30-alpine
COPY --from=build /app/dist /usr/share/nginx/html

EXPOSE 80
