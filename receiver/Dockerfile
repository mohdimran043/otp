# The receiver: the Go binary and the built browser app, behind nginx.
#
# Three stages, and the split is deliberate. The web build and the Go build share nothing, so they run
# independently and a change to one does not invalidate the other's cache. The final image carries neither
# toolchain.

# --- the browser app ---------------------------------------------------------
FROM node:22-alpine AS web
WORKDIR /web

# The manifest first, so a change to the source does not re-resolve the dependency graph.
COPY receiver/web/package.json receiver/web/package-lock.json* ./
RUN npm install --no-audit --no-fund

COPY receiver/web/ ./
# Type-checked as part of the build rather than in a separate step, so a type error fails the image rather
# than shipping in it.
RUN npm run build

# --- the Go binary -----------------------------------------------------------
FROM golang:1.26-bookworm AS build
WORKDIR /src

# The shared module is copied first and separately, because it is the dependency: the receiver's own sources
# change far more often, and this way its rebuild does not re-download the module graph.
COPY shared/ ./shared/
COPY receiver/go.mod receiver/go.sum ./receiver/
WORKDIR /src/receiver
RUN go mod download

COPY receiver/ ./
# Statically linked, so the runtime image needs no libc and the binary can be dropped into a distroless or
# scratch image if a deployment prefers one.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/receiver ./cmd/receiver

# --- the runtime -------------------------------------------------------------
FROM nginx:1.27-alpine

RUN apk add --no-cache ca-certificates tzdata su-exec

COPY --from=build /out/receiver /usr/local/bin/receiver
COPY --from=web /web/dist /usr/share/nginx/html
COPY receiver/nginx.conf /etc/nginx/conf.d/default.conf
# The admin paths are shared by both applications, so they live in one file rather than two that drift.
COPY deploy/nginx-admin.conf /etc/nginx/admin.conf
# Empty, and deliberately so: the TLS include globs this directory, matching nothing in development.
# A deployment mounts its certificate and server block over it. See deploy/tls/https.conf.example.
RUN mkdir -p /etc/nginx/tls
COPY receiver/docker-entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# The API listens only on the loopback interface inside the container, so nginx is the only way in. That is
# what makes the proxy a boundary rather than a convenience: the application cannot be reached around it.
ENV OTP_RECEIVER_HOST=127.0.0.1 \
    OTP_RECEIVER_PORT=8081

EXPOSE 80
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
