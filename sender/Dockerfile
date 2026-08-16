# The sender: the Go binary and the built browser app, behind nginx.
#
# Three stages, and the split is deliberate. The web build and the Go build share nothing, so they run
# independently and a change to one does not invalidate the other's cache. The final image carries neither
# toolchain.

# --- the browser app ---------------------------------------------------------
FROM node:22-alpine AS web
WORKDIR /web

# The manifest first, so a change to the source does not re-resolve the dependency graph.
COPY sender/web/package.json sender/web/package-lock.json* ./
RUN npm install --no-audit --no-fund

COPY sender/web/ ./
# Type-checked as part of the build rather than in a separate step, so a type error fails the image rather
# than shipping in it.
RUN npm run build

# --- the Go binary -----------------------------------------------------------
FROM golang:1.26-bookworm AS build
WORKDIR /src

# The shared module is copied first and separately, because it is the dependency: the sender's own sources
# change far more often, and this way its rebuild does not re-download the module graph.
COPY shared/ ./shared/
COPY sender/go.mod sender/go.sum ./sender/
WORKDIR /src/sender
RUN go mod download

COPY sender/ ./
# Statically linked, so the runtime image needs no libc and the binary can be dropped into a distroless or
# scratch image if a deployment prefers one.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sender ./cmd/sender

# --- the proxy ---------------------------------------------------------------
#
# nginx is its own image and its own container, rather than a second process beside the API.
#
# It used to be one container running both, on the argument that the proxy exists to be the only route to
# this particular API and so the two are one unit. That is a fair argument and it lost to an operational one:
# every change to a proxy setting — a timeout, a header, a certificate, an admin path — meant rebuilding and
# redeploying the application image to alter a file the application never reads. In production that is the
# difference between restarting a proxy and rolling the service.
#
# So the config is mounted rather than baked, and changing it costs `docker compose restart nginx`. What is
# baked is the built browser app, because that genuinely is part of a release: the assets and the API that
# serves them are versioned together and a mismatched pair is a broken page.
FROM nginx:1.27-alpine AS proxy

COPY --from=web /web/dist /usr/share/nginx/html

# Present and empty. The TLS include globs this directory, matching nothing in development; a deployment
# mounts its certificate and server block over it. See deploy/tls/https.conf.example.
RUN mkdir -p /etc/nginx/tls

EXPOSE 80

# --- the runtime -------------------------------------------------------------
FROM alpine:3.21 AS app

RUN apk add --no-cache ca-certificates tzdata wget

COPY --from=build /out/sender /usr/local/bin/sender
COPY sender/docker-entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# Bound to every interface inside the container, and reachable only from the compose network: the port is
# not published, so the proxy remains the only way in from the host. It was 127.0.0.1 when nginx shared the
# container, which is a stronger boundary and is not available once they are separate containers.
ENV OTP_SENDER_HOST=0.0.0.0 \
    OTP_SENDER_PORT=8080

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
