# The binary is built outside the image and copied in from
# <TARGETPLATFORM>/iidp-deploy-gate, the layout both GoReleaser's dockers_v2
# and the kind harness give the build context. git is the gate's only runtime
# dependency: it clones and pushes the Platform repository.
FROM docker.io/library/alpine:3.22
RUN apk add --no-cache git ca-certificates
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/iidp-deploy-gate /usr/local/bin/iidp-deploy-gate
USER 65532:65532
ENV HOME=/tmp
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/iidp-deploy-gate"]
