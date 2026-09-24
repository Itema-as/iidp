# The Deploy gate's image. The binary is built outside the image and copied
# in from <TARGETPLATFORM>/iidp-deploy-gate: GoReleaser's dockers_v2 lays
# the build context out that way (.goreleaser.yaml), and the kind harness
# does the same with a binary it cross-compiles (test/e2e/harness.go), so
# both build from this one file. git is the gate's only runtime dependency:
# it clones and pushes the Platform repository the way the CLI does
# (internal/git).
FROM docker.io/library/alpine:3.22
RUN apk add --no-cache git ca-certificates
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/iidp-deploy-gate /usr/local/bin/iidp-deploy-gate
USER 65532:65532
ENV HOME=/tmp
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/iidp-deploy-gate"]
