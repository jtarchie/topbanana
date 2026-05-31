FROM golang:alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags='-s -w' -o /out/topbanana ./cmd/topbanana

FROM alpine
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/topbanana /usr/local/bin/topbanana
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
# Allow the non-root binary to bind 80/443 for autocert without running as
# root. Required on Fly machines, which drop to an unprivileged user.
RUN apk add --no-cache libcap && setcap 'cap_net_bind_service=+ep' /usr/local/bin/topbanana
EXPOSE 80 443
ENTRYPOINT ["/entrypoint.sh"]
