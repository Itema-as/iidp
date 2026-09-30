# Dockerfile stub for {{.Name}}. iidp could not generate a working image for
# an "Other" framework; fill this in before the first deploy. Until you do,
# the deploy workflow's build step fails.
#
# Fill in, in order, above the USER line at the end:
#
#   1. FROM a base image for your language or runtime
#      (for example: python:3.13-slim, golang:1.23-alpine, ruby:3.3-slim).
#
#   2. Copy your source and install dependencies.
#
#   3. Build your application, if it needs a build step.
#
#   4. EXPOSE the port your application listens on.
#
#   5. Set the command that starts your application.
#
# Your application MUST listen on the port named by the PORT environment
# variable, which the Platform sets at runtime. Do not hardcode a port.
#
# It MUST run as a non-root user, given by number: the Platform refuses to
# start a container as root, and can only check a numeric USER. Keep the
# USER line last in the file. 1000 needs no user in the image; if your base
# image has a non-root user of its own, you can give its number instead.
#
# Example shape for a server that reads PORT itself:
#
#   FROM golang:1.23-alpine AS build
#   WORKDIR /app
#   COPY . .
#   RUN go build -o server .
#
#   FROM alpine:3.20
#   COPY --from=build /app/server /server
#   EXPOSE 8080
#   CMD ["/server"]
#   USER 1000:1000

USER 1000:1000
