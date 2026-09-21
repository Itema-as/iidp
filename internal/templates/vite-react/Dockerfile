# Multi-stage build: build the static site with Vite, then serve dist/ with
# nginx. A Static site listens on 80; the Platform sets no PORT for it.

ARG NODE_VERSION=24-slim

FROM node:${NODE_VERSION} AS build
WORKDIR /app
COPY package.json ./
RUN npm install
COPY . .
RUN npm run build

FROM nginx:1.27-alpine
COPY --from=build /app/dist /usr/share/nginx/html

EXPOSE 80
