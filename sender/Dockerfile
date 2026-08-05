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

# --- the runtime -------------------------------------------------------------
FROM nginx:1.27-alpine

RUN apk add --no-cache ca-certificates tzdata su-exec

COPY --from=build /out/sender /usr/local/bin/sender
COPY --from=web /web/dist /usr/share/nginx/html
COPY sender/nginx.conf /etc/nginx/conf.d/default.conf
COPY sender/docker-entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# The API listens only on the loopback interface inside the container, so nginx is the only way in. That is
# what makes the proxy a boundary rather than a convenience: the application cannot be reached around it.
ENV OTP_SENDER_HOST=127.0.0.1 \
    OTP_SENDER_PORT=8080

EXPOSE 80
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
