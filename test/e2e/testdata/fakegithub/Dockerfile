# The kind harness's stand-in for GitHub. The harness cross-compiles the
# binary into <TARGETPLATFORM>/fakegithub, as for the Deploy gate's image.
FROM docker.io/library/alpine:3.22
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/fakegithub /usr/local/bin/fakegithub
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/fakegithub"]
