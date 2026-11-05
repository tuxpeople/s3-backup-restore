FROM golang:1.27.3-trixie AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -ldflags="-s -w" -o /bin/s3-backup-restore ./cmd/s3-backup-restore

FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /bin/s3-backup-restore /bin/s3-backup-restore
USER 65532:65532

ENTRYPOINT ["/bin/s3-backup-restore"]
CMD ["cron"]
