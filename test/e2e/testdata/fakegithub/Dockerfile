# The kind harness's stand-in for GitHub (main.go in this directory). The
# harness cross-compiles the binary into <TARGETPLATFORM>/fakegithub, the
# same layout as the Deploy gate's own image (cmd/iidp-deploy-gate), and
# builds from the Alpine base the git server already pulled. Never
# published.
FROM docker.io/library/alpine:3.22
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/fakegithub /usr/local/bin/fakegithub
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/fakegithub"]
