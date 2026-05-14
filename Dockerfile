FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags='-s -w' -o /out/buildabear ./cmd/buildabear

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/buildabear /usr/local/bin/buildabear
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
EXPOSE 8080
ENTRYPOINT ["/entrypoint.sh"]
