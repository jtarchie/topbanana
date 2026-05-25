FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags='-s -w' -o /out/bloomhollow ./cmd/bloomhollow

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/bloomhollow /usr/local/bin/bloomhollow
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
# Allow the non-root binary to bind 80/443 for autocert without running as
# root. Required on Fly machines, which drop to an unprivileged user.
RUN apk add --no-cache libcap && setcap 'cap_net_bind_service=+ep' /usr/local/bin/bloomhollow
EXPOSE 80 443
ENTRYPOINT ["/entrypoint.sh"]
