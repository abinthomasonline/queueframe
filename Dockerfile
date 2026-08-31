# syntax=docker/dockerfile:1@sha256:87999aa3d42bdc6bea60565083ee17e86d1f3339802f543c0d03998580f9cb89

FROM golang:1.26.5-bookworm@sha256:6c5605ab3a9a9fb3c4eafe5b3d63cdbf3881caf113262b67862547b54a9db599 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -mod=readonly -buildvcs=false -trimpath -ldflags="-s -w" \
    -o /out/queueframe ./cmd/queueframe

FROM debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171 AS runtime

LABEL org.opencontainers.image.title="Queueframe" \
      org.opencontainers.image.description="Self-hosted scheduled order dispatch for Zerodha Kite" \
      org.opencontainers.image.source="https://github.com/abinthomasonline/queueframe" \
      org.opencontainers.image.licenses="Apache-2.0"

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 queueframe \
    && useradd --uid 10001 --gid queueframe --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin queueframe \
    && install -d -o queueframe -g queueframe /app /data

COPY --from=build --chown=queueframe:queueframe /out/queueframe /usr/local/bin/queueframe
COPY --chown=queueframe:queueframe LICENSE THIRD_PARTY_NOTICES.md /usr/share/licenses/queueframe/
COPY --chown=queueframe:queueframe third_party/licenses /usr/share/licenses/queueframe/third_party/
COPY --chown=queueframe:queueframe internal/web/static/icons-LICENSE.txt /usr/share/licenses/queueframe/assets/
COPY --chown=queueframe:queueframe internal/web/static/fonts/IBMPlex-LICENSE.txt internal/web/static/fonts/InstrumentSans-LICENSE.txt /usr/share/licenses/queueframe/assets/

WORKDIR /app
USER queueframe:queueframe

ENV TZ=Asia/Kolkata \
    QUEUEFRAME_ADDR=:8080 \
    QUEUEFRAME_DB=/data/queueframe.db

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/queueframe"]
