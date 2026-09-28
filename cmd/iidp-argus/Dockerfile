# Argus's image. The binary is built outside the image and copied in from
# <TARGETPLATFORM>/iidp-argus, the layout GoReleaser's dockers_v2 gives the
# build context (.goreleaser.yaml), as for the Deploy gate. Argus needs
# nothing at run time besides itself: the frontend is embedded in it, it
# carries its own time zone data, and it trusts the API server with the
# service account's CA. The base is the gate's, which the node already has.
FROM docker.io/library/alpine:3.22
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/iidp-argus /usr/local/bin/iidp-argus
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/iidp-argus"]
