# The binary is copied in from <TARGETPLATFORM>/iidp-argus, the layout
# GoReleaser's dockers_v2 gives the build context. Argus needs nothing else at
# run time: the frontend is embedded, it carries its own time zone data, and it
# trusts the API server through the service account's CA. The base is the
# Deploy gate's, which the node already has.
FROM docker.io/library/alpine:3.22
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/iidp-argus /usr/local/bin/iidp-argus
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/iidp-argus"]
