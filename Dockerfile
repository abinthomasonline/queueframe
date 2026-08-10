# syntax=docker/dockerfile:1@sha256:87999aa3d42bdc6bea60565083ee17e86d1f3339802f543c0d03998580f9cb89

FROM golang:1.26.5-bookworm@sha256:6c5605ab3a9a9fb3c4eafe5b3d63cdbf3881caf113262b67862547b54a9db599 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -mod=readonly -buildvcs=false -trimpath -ldflags="-s -w" \
    -o /out/queueframe ./cmd/queueframe

FROM debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241 AS runtime

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 queueframe \
    && useradd --uid 10001 --gid queueframe --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin queueframe \
    && install -d -o queueframe -g queueframe /app /data

COPY --from=build --chown=queueframe:queueframe /out/queueframe /usr/local/bin/queueframe

WORKDIR /app
USER queueframe:queueframe

ENV TZ=Asia/Kolkata \
    QUEUEFRAME_ADDR=:8080 \
    QUEUEFRAME_DB=/data/queueframe.db

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/queueframe"]
