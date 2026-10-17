# syntax=docker/dockerfile:1
FROM golang:1.27.2-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-buildvcs=false
# Dependencies first, into the image layer (not a cache mount): the layer survives source-only
# changes and is exported with the build cache (CI's type=gha), which cache mounts are not.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The compile cache is a mount: it only speeds up local rebuilds.
RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 app \
    && adduser -S -D -H -u 10001 -G app app
COPY --from=build /out/server /usr/local/bin/server
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
