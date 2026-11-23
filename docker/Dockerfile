# Build on the runner's platform and cross-compile: no QEMU emulation needed
FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" GOTOOLCHAIN=local \
    go build -trimpath -ldflags="-s -w" -o /bin/s3-backup-restore ./cmd/s3-backup-restore

FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5

LABEL org.opencontainers.image.title="s3-backup-restore"
LABEL org.opencontainers.image.description="Back up a folder to S3 on a schedule and restore it"
LABEL org.opencontainers.image.source="https://github.com/tuxpeople/s3-backup-restore"

COPY --from=builder /bin/s3-backup-restore /bin/s3-backup-restore
USER 65532:65532

ENTRYPOINT ["/bin/s3-backup-restore"]
CMD ["cron"]
