FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go test ./... && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/quota-sync ./cmd/sub2api-autoreset

FROM alpine:3.22
ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="Sub2API-AutoReset" \
      org.opencontainers.image.description="OpenAI OAuth quota reset synchronization sidecar for Sub2API" \
      org.opencontainers.image.source="https://github.com/wellblink/Sub2API-AutoReset" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${BUILD_DATE}"
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -g 10001 -S quota-sync && adduser -u 10001 -S -G quota-sync quota-sync
COPY --from=build /out/quota-sync /usr/local/bin/quota-sync
USER 10001:10001
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/quota-sync"]
