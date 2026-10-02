package apprepo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Itema-as/iidp/internal/platformrepo"
)

// helloDockerfile runs as a named user, created with its number.
const helloDockerfile = `ARG NODE_VERSION=24-slim

FROM node:${NODE_VERSION} AS deps
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci

FROM node:${NODE_VERSION} AS runner
WORKDIR /app
RUN addgroup --system --gid 1001 nodejs \
  && adduser --system --uid 1001 nextjs
COPY --from=deps --chown=nextjs:nodejs /app/node_modules ./node_modules

USER nextjs

EXPOSE 3000

CMD ["node", "server.js"]
`

func TestFixNonRoot(t *testing.T) {
	const (
		web    = platformrepo.KindWebService
		static = platformrepo.KindStaticSite
	)
	for _, tc := range []struct {
		name, dockerfile, kind string
		// want is the fixed Dockerfile, "" when Adopt leaves it as it is.
		want string
		// advice is part of the Advice wanted, "" for none.
		advice string
	}{
		// Already non-root: nothing to change or say.
		{name: "numeric user and group", kind: web, dockerfile: "FROM node:24-slim\nUSER 1000:1000\nCMD [\"node\"]\n"},
		{name: "numeric user", kind: web, dockerfile: "FROM python:3.13\nUSER 1001\n"},
		{name: "the unprivileged nginx", kind: static, dockerfile: "FROM nginxinc/nginx-unprivileged:1.30-alpine\nCOPY dist /usr/share/nginx/html\n"},
		{name: "the unprivileged nginx on Docker Hub", kind: static, dockerfile: "FROM docker.io/nginxinc/nginx-unprivileged:1.30-alpine\n"},
		{name: "a root build stage before a non-root final one", kind: static, dockerfile: "FROM node:24 AS build\nRUN npm run build\n\nFROM nginxinc/nginx-unprivileged:1.30-alpine\nCOPY --from=build /app/dist /usr/share/nginx/html\n"},
		{name: "USER inherited from the stage the final one is built FROM", kind: web, dockerfile: "FROM node:24 AS base\nUSER 1000\n\nFROM base\nCMD [\"node\"]\n"},
		{name: "the last USER counts", kind: web, dockerfile: "FROM node:24\nUSER root\nRUN apt-get update\nUSER 1000:1000\n"},

		// A Node base image running as root: the node user, by number.
		{
			name: "node without USER", kind: web,
			dockerfile: "FROM node:24-slim\nWORKDIR /app\nCOPY . .\nRUN npm ci\nEXPOSE 3000\nCMD [\"node\", \"server.js\"]\n",
			want:       "FROM node:24-slim\nWORKDIR /app\nCOPY . .\nRUN npm ci\n\n# Non-root, by number: the Platform starts no container as root.\nUSER 1000:1000\n\nEXPOSE 3000\nCMD [\"node\", \"server.js\"]\n",
		},
		{
			name: "node with USER root, kept for the RUN after it", kind: web,
			dockerfile: "FROM node:24\nUSER root\nRUN apt-get install -y curl\nCMD [\"node\"]\n",
			want:       "FROM node:24\nUSER root\nRUN apt-get install -y curl\n\n# Non-root, by number: the Platform starts no container as root.\nUSER 1000:1000\n\nCMD [\"node\"]\n",
		},
		{
			name: "node with USER 0", kind: web,
			dockerfile: "FROM node:24\nUSER 0\n",
			want:       "FROM node:24\nUSER 0\n\n# Non-root, by number: the Platform starts no container as root.\nUSER 1000:1000\n",
		},
		{
			name: "node with USER 0:0", kind: web,
			dockerfile: "FROM node:24\nUSER 0:0\nCMD [\"node\"]\n",
			want:       "FROM node:24\nUSER 0:0\n\n# Non-root, by number: the Platform starts no container as root.\nUSER 1000:1000\n\nCMD [\"node\"]\n",
		},
		{
			name: "node named in full, from a build argument, lower-case instructions", kind: web,
			dockerfile: "ARG NODE_VERSION=24-slim\nfrom docker.io/library/node:${NODE_VERSION}\ncmd [\"node\"]\n",
			want:       "ARG NODE_VERSION=24-slim\nfrom docker.io/library/node:${NODE_VERSION}\n\n# Non-root, by number: the Platform starts no container as root.\nUSER 1000:1000\n\ncmd [\"node\"]\n",
		},
		{
			name: "node with the comment above CMD kept with it", kind: web,
			dockerfile: "FROM node:24\nCOPY . .\n# Start the server.\n# It reads PORT.\nCMD [\"node\"]\n",
			want:       "FROM node:24\nCOPY . .\n\n# Non-root, by number: the Platform starts no container as root.\nUSER 1000:1000\n\n# Start the server.\n# It reads PORT.\nCMD [\"node\"]\n",
		},
		{
			name: "node with a here-document", kind: web,
			dockerfile: "FROM node:24\nRUN <<EOF\nnpm ci\nCMD not an instruction\nEOF\nCMD [\"node\"]\n",
			want:       "FROM node:24\nRUN <<EOF\nnpm ci\nCMD not an instruction\nEOF\n\n# Non-root, by number: the Platform starts no container as root.\nUSER 1000:1000\n\nCMD [\"node\"]\n",
		},
		{
			name: "node, the final stage of several", kind: web,
			dockerfile: "FROM node:24 AS build\nRUN npm run build\n\nFROM node:24-slim\nCOPY --from=build /app /app\nCMD [\"node\", \"/app\"]\n",
			want:       "FROM node:24 AS build\nRUN npm run build\n\nFROM node:24-slim\nCOPY --from=build /app /app\n\n# Non-root, by number: the Platform starts no container as root.\nUSER 1000:1000\n\nCMD [\"node\", \"/app\"]\n",
		},
		{
			name: "node's own named user", kind: web,
			dockerfile: "FROM node:24\nUSER node\nCMD [\"node\"]\n",
			want:       "FROM node:24\nUSER 1000:1000\nCMD [\"node\"]\n",
		},

		// A named user whose number the Dockerfile shows.
		{
			name: "hello: adduser --uid", kind: web,
			dockerfile: helloDockerfile,
			want:       strings.Replace(helloDockerfile, "USER nextjs", "USER 1001", 1),
		},
		{
			name: "useradd -u on any base", kind: web,
			dockerfile: "FROM python:3.13-slim\nRUN useradd -m -u 1234 -s /bin/sh app\nUSER app\nCMD [\"python\"]\n",
			want:       "FROM python:3.13-slim\nRUN useradd -m -u 1234 -s /bin/sh app\nUSER 1234\nCMD [\"python\"]\n",
		},
		{
			name: "adduser --uid= with a group kept", kind: web,
			dockerfile: "FROM alpine:3.22\nRUN adduser -D --uid=900 app; echo done\nUSER app:app\n",
			want:       "FROM alpine:3.22\nRUN adduser -D --uid=900 app; echo done\nUSER 900:app\n",
		},
		{
			name: "busybox adduser -u in the stage the final one is built FROM", kind: web,
			dockerfile: "FROM alpine:3.22 AS base\nRUN /usr/sbin/adduser -D -u 70 app\n\nFROM base\n  USER app\n",
			want:       "FROM alpine:3.22 AS base\nRUN /usr/sbin/adduser -D -u 70 app\n\nFROM base\n  USER 70\n",
		},

		// nginx for a Static site: the unprivileged nginx, on 8080.
		{
			name: "nginx", kind: static,
			dockerfile: "# The page.\nFROM nginx:1.30-alpine\n\nCOPY index.html /usr/share/nginx/html/\nCOPY assets /usr/share/nginx/html/assets\n\nEXPOSE 80\n",
			want:       "# The page.\nFROM nginxinc/nginx-unprivileged:1.30-alpine\n\nCOPY index.html /usr/share/nginx/html/\nCOPY assets /usr/share/nginx/html/assets\n\nEXPOSE 8080\n",
		},
		{
			name: "nginx named in full, with a platform and a stage name", kind: static,
			dockerfile: "FROM --platform=linux/amd64 docker.io/library/nginx:stable-alpine AS web\nEXPOSE 80/tcp\n",
			want:       "FROM --platform=linux/amd64 nginxinc/nginx-unprivileged:stable-alpine AS web\nEXPOSE 8080/tcp\n",
		},
		{
			name: "nginx without a tag", kind: static,
			dockerfile: "FROM nginx\nCOPY dist /usr/share/nginx/html\n",
			want:       "FROM nginxinc/nginx-unprivileged\nCOPY dist /usr/share/nginx/html\n",
		},
		{
			name: "nginx with its tag from a build argument", kind: static,
			dockerfile: "ARG NGINX=1.30-alpine\nFROM nginx:${NGINX}\n",
			want:       "ARG NGINX=1.30-alpine\nFROM nginxinc/nginx-unprivileged:${NGINX}\n",
		},
		{
			name: "nginx after a node build stage", kind: static,
			dockerfile: "FROM node:24 AS build\nRUN npm run build\n\nFROM nginx:1.30-alpine\nCOPY --from=build /app/dist /usr/share/nginx/html\nEXPOSE 80\n",
			want:       "FROM node:24 AS build\nRUN npm run build\n\nFROM nginxinc/nginx-unprivileged:1.30-alpine\nCOPY --from=build /app/dist /usr/share/nginx/html\nEXPOSE 8080\n",
		},

		// No known fix: left as it is, with what to change.
		{name: "an unknown base image", kind: web, dockerfile: "FROM python:3.13-slim\nCMD [\"python\"]\n", advice: "who its base image (`python:3.13-slim`) runs as"},
		{name: "a node image from another registry", kind: web, dockerfile: "FROM ghcr.io/acme/node:24\n", advice: "`ghcr.io/acme/node:24`"},
		{name: "scratch", kind: web, dockerfile: "FROM scratch\nCOPY server /\n", advice: "`scratch`"},
		{name: "USER root on an unknown base", kind: web, dockerfile: "FROM python:3.13\nUSER root\n", advice: "runs as root (`USER root`)"},
		{name: "a named user without its number", kind: web, dockerfile: "FROM python:3.13\nUSER app\n", advice: "`USER app`, and the Dockerfile doesn't show that user's number"},
		{name: "a user from a build argument", kind: web, dockerfile: "FROM node:24\nARG UID=1000\nUSER ${UID}\n", advice: "`USER ${UID}`, which iidp can't resolve"},
		{name: "two numbers for one user", kind: web, dockerfile: "FROM alpine\nRUN adduser -D -u 1 app\nRUN adduser -D -u 2 app\nUSER app\n", advice: "`USER app`"},
		{name: "a node user of the Dockerfile's own, without a number", kind: web, dockerfile: "FROM python:3.13\nRUN useradd node\nUSER node\n", advice: "`USER node`"},
		{name: "nginx that runs commands", kind: static, dockerfile: "FROM nginx:1.30-alpine\nRUN rm /etc/nginx/conf.d/default.conf\n", advice: "runs commands (`RUN`)"},
		{name: "nginx with a config of its own", kind: static, dockerfile: "FROM nginx:1.30-alpine\nCOPY nginx.conf /etc/nginx/conf.d/default.conf\n", advice: "nginx configuration of its own"},
		{name: "nginx as a Web service", kind: web, dockerfile: "FROM nginx:1.30-alpine\n", advice: "a Web service listens on its Environment's `port`"},
		{name: "nginx by digest", kind: static, dockerfile: "FROM nginx@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n", advice: "pinned by digest"},
		{name: "nginx asking for root", kind: static, dockerfile: "FROM nginx:1.30-alpine\nUSER root\n", advice: "asks for root (`USER root`)"},
		{name: "nginx named by a build argument", kind: static, dockerfile: "ARG BASE=nginx:1.30-alpine\nFROM ${BASE}\n", advice: "comes from a build argument"},
		{name: "no FROM", kind: web, dockerfile: "# nothing yet\nUSER 1000:1000\n", advice: "no FROM"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := fixNonRoot([]byte(tc.dockerfile), tc.kind)
			if string(got.Dockerfile) != tc.want {
				t.Errorf("Dockerfile:\n%s\nwant:\n%s", got.Dockerfile, tc.want)
			}
			if (tc.want != "") != (got.Change != "") {
				t.Errorf("Change = %q, want one exactly when the Dockerfile changes", got.Change)
			}
			switch {
			case tc.advice == "" && got.Advice != "":
				t.Errorf("Advice = %q, want none", got.Advice)
			case !strings.Contains(got.Advice, tc.advice):
				t.Errorf("Advice = %q, want it to contain %q", got.Advice, tc.advice)
			}
		})
	}
}

func TestTheTemplatesDockerfilesNeedNoFix(t *testing.T) {
	for framework, kind := range map[string]string{"nextjs": platformrepo.KindWebService, "vite-react": platformrepo.KindStaticSite} {
		dockerfile, err := os.ReadFile(filepath.Join("..", "templates", framework, "Dockerfile"))
		if err != nil {
			t.Fatal(err)
		}
		if got := fixNonRoot(dockerfile, kind); got.Dockerfile != nil || got.Advice != "" {
			t.Errorf("%s: %+v, want nothing to change or say", framework, got)
		}
	}
}

func TestFixNonRootSaysWhereTheNumberComesFrom(t *testing.T) {
	got := fixNonRoot([]byte(helloDockerfile), platformrepo.KindWebService)
	if want := "`USER nextjs` becomes `USER 1001`, the number the Dockerfile gives it (`adduser --uid 1001`)"; !strings.HasPrefix(got.Change, want) {
		t.Errorf("Change = %q, want it to start with %q", got.Change, want)
	}
}
