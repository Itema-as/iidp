# The binary is built outside the image and copied in from
# <TARGETPLATFORM>/iidp-db-tunnel, the layout both GoReleaser's dockers_v2
# and the kind harness give the build context. git is the tunnel's only
# runtime dependency: each check clones the Platform repository with the
# developer's token. The base is the Deploy gate's, which the node already
# has.
FROM docker.io/library/alpine:3.22
RUN apk add --no-cache git ca-certificates
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/iidp-db-tunnel /usr/local/bin/iidp-db-tunnel
USER 65532:65532
ENV HOME=/tmp
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/iidp-db-tunnel"]
