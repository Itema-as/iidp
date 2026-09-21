# A git smart-HTTP server for the kind harness: git-http-backend behind
# lighttpd's CGI module, serving the bare repositories the harness unpacks
# into /srv/git. Built and loaded into the kind cluster by test/e2e; never
# published.
FROM docker.io/library/alpine:3.22
RUN apk add --no-cache git git-daemon lighttpd
COPY lighttpd.conf /etc/lighttpd/lighttpd.conf
EXPOSE 8080
CMD ["lighttpd", "-D", "-f", "/etc/lighttpd/lighttpd.conf"]
