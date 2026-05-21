FROM golang:1.22-alpine AS builder

WORKDIR /src

ARG LOOPER_BUILD_VERSION
ARG LOOPER_BUILD_VERSION_SOURCE
ARG LOOPER_BUILD_CHANNEL
ARG LOOPER_BUILD_API_VERSION
ARG LOOPER_BUILD_GIT_SHA
ARG LOOPER_BUILD_TIMESTAMP

# CGO is required because loopernet uses mattn/go-sqlite3.
RUN apk add --no-cache build-base ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN mkdir -p /out \
	&& export LOOPER_BUILD_VERSION="$LOOPER_BUILD_VERSION" \
	&& export LOOPER_BUILD_VERSION_SOURCE="$LOOPER_BUILD_VERSION_SOURCE" \
	&& export LOOPER_BUILD_CHANNEL="$LOOPER_BUILD_CHANNEL" \
	&& export LOOPER_BUILD_API_VERSION="$LOOPER_BUILD_API_VERSION" \
	&& export LOOPER_BUILD_GIT_SHA="$LOOPER_BUILD_GIT_SHA" \
	&& export LOOPER_BUILD_TIMESTAMP="$LOOPER_BUILD_TIMESTAMP" \
	&& CGO_ENABLED=1 go build -trimpath -ldflags "$(go run ./tools/go-build-flags)" -o /out/loopernet ./cmd/loopernet

FROM alpine:3.20

RUN apk add --no-cache ca-certificates \
	&& addgroup -S loopernet \
	&& adduser -S -G loopernet -h /var/lib/loopernet loopernet \
	&& mkdir -p /var/lib/loopernet \
	&& chown -R loopernet:loopernet /var/lib/loopernet

COPY --from=builder /out/loopernet /usr/local/bin/loopernet

ENV LOOPERNET_LISTEN_ADDR=0.0.0.0:8089
ENV LOOPERNET_DB_PATH=/var/lib/loopernet/loopernet.sqlite

VOLUME ["/var/lib/loopernet"]
EXPOSE 8089

USER loopernet
ENTRYPOINT ["/usr/local/bin/loopernet"]
