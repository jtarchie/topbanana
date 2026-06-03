FROM golang:alpine AS builder
WORKDIR /src
# Tailwind v4 standalone CLI (Node-free, musl-static) for the runtime per-site
# CSS compile. Downloaded in the builder and copied into the runtime image so
# the final image carries no npm/node. The embedded admin sheet
# (internal/assets/app.css) is committed and compiled separately via `task css`.
ARG TAILWIND_VERSION=v4.3.0
RUN apk add --no-cache curl && \
    case "$(uname -m)" in \
      x86_64) tw=tailwindcss-linux-x64-musl ;; \
      aarch64) tw=tailwindcss-linux-arm64-musl ;; \
      *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;; \
    esac && \
    curl -fsSL "https://github.com/tailwindlabs/tailwindcss/releases/download/${TAILWIND_VERSION}/${tw}" -o /usr/local/bin/tailwindcss && \
    chmod +x /usr/local/bin/tailwindcss
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags='-s -w' -o /out/topbanana ./cmd/topbanana

FROM alpine
# libstdc++/libgcc: the Tailwind standalone "musl" build is not fully static —
# it dynamically links the C++ runtime, so it fails to relocate on bare alpine.
RUN apk add --no-cache ca-certificates libstdc++ libgcc
COPY --from=builder /out/topbanana /usr/local/bin/topbanana
# tailwindcss on PATH — build.Service.optimizeCSS resolves it for the per-site
# compile. If absent, sites fall back to keeping their CDN substrate tags.
COPY --from=builder /usr/local/bin/tailwindcss /usr/local/bin/tailwindcss
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
# Allow the non-root binary to bind 80/443 for autocert without running as
# root. Required on Fly machines, which drop to an unprivileged user.
RUN apk add --no-cache libcap && setcap 'cap_net_bind_service=+ep' /usr/local/bin/topbanana
EXPOSE 80 443
ENTRYPOINT ["/entrypoint.sh"]
