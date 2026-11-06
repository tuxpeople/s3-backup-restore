# Build on the runner's platform and cross-compile: no QEMU emulation needed
FROM --platform=$BUILDPLATFORM golang:1.27.3-trixie@sha256:af1bfc2614ebb1dbd31af514b78740fd678850e76498762b425ecee4c5c6ea80 AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" GOTOOLCHAIN=local \
    go build -trimpath -ldflags="-s -w" -o /bin/s3-backup-restore ./cmd/s3-backup-restore

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329

LABEL org.opencontainers.image.title="s3-backup-restore"
LABEL org.opencontainers.image.description="Back up a folder to S3 on a schedule and restore it"
LABEL org.opencontainers.image.source="https://github.com/tuxpeople/s3-backup-restore"

COPY --from=builder /bin/s3-backup-restore /bin/s3-backup-restore
USER 65532:65532

ENTRYPOINT ["/bin/s3-backup-restore"]
CMD ["cron"]
