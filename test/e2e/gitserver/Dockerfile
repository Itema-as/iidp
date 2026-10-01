# The kind harness's git smart-HTTP server: git-http-backend behind
# lighttpd's CGI module, serving the bare repositories in /srv/git.
FROM docker.io/library/alpine:3.22
RUN apk add --no-cache git git-daemon lighttpd
COPY lighttpd.conf /etc/lighttpd/lighttpd.conf
EXPOSE 8080
CMD ["lighttpd", "-D", "-f", "/etc/lighttpd/lighttpd.conf"]
