# S3 Backup and Restore
A docker image for backing up and restoring data from s3.

## Origin
This repository was cloned from the [`s3-backup-restore` subdirectory](https://github.com/bloveless/docker-images/tree/main/s3-backup-restore)
of the [bloveless/docker-images](https://github.com/bloveless/docker-images) repository.

## Description
Automatic backups to s3. Automatic restores from s3 if the volume is completely empty (configurable).
Allow forced restores which will overwrite everything in the mounted volume.
Force restores can be done either for a specific time or the latest.

## Build and runtime
Build the current image with:

```sh
docker build -f docker/Dockerfile -t s3-backup-restore:local .
```

The build uses Go 1.27.3 and AWS SDK for Go v2. The runtime uses Distroless Debian 13
and runs as UID/GID `65532:65532` by default. Give that user access to the mounted data
directory and mounted secrets, or select the appropriate UID/GID with `--user` (Docker)
or `runAsUser` / `runAsGroup` (Kubernetes).

`CHOWN_ENABLE=true` requires permission to change ownership. When restoring to a different
UID/GID, explicitly run the restore container as root; backup containers can remain unprivileged.
The development Compose service runs the builder image and is intended for local development.

## Backup and restore safety
- Only regular files and directories are supported. Backups fail on symbolic links and special
  files rather than following them or silently creating an incomplete archive. Restore rejects
  archive links, special files, unsafe paths, and existing symlinks in the target path.
- Restore replaces each file using a temporary file inside the data directory. Existing hardlinks
  are replaced without modifying their other targets. Ensure enough free space for the downloaded
  archive and a replacement of the largest restored file. An interrupted or failed restore can leave
  a mixture of restored and existing files; the entire restore is not transactional.
- Failed backups do not prune older backups. Each run owns and removes its own temporary archive.
  Overlapping backup jobs in one process are skipped. Run only one backup process per bucket/prefix;
  the process lock does not coordinate separate containers. Use filesystem snapshots or pause writers
  when application-consistent backups are required.
- Keep S3 write permissions restricted to trusted backup writers. Restore does not authenticate
  archives independently of S3. Bucket versioning and a separately configured Object Lock policy
  can provide additional protection against backup deletion.
- `-v` and `-vv` enable application debug output without AWS HTTP headers or bodies.
- Use workload IAM roles where available. AWS environment credentials, shared configuration,
  and `/run/secrets/AWS_ACCESS_KEY_ID`, `/run/secrets/AWS_SECRET_ACCESS_KEY`, and
  `/run/secrets/AWS_SESSION_TOKEN` are also supported.

## Goals
- [x] Accept credentials through docker secrets and env variables.
- [x] Backup to s3 on some cadence (configurable).
- [x] If the directory is empty then restore from the latest backup automatically.
- [x] Separate sub-commands for backup, restore, and cron.
- [x] Clean up backups.

## Config
If you use access keys, pass `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` through environment variables
or Docker secrets under `/run/secrets/`. Temporary credentials also require `AWS_SESSION_TOKEN`.
IAM roles and shared AWS configuration are supported by the SDK's default credential chain.

`S3_BUCKET` is required. Configure an AWS region through `AWS_REGION` or shared AWS configuration.
Credentials may come from an IAM role, AWS environment variables, shared configuration, or Docker secrets.

You can configure the backup and restore using the following environment variables:

`AWS_REGION`: The AWS region you are targeting. I.E. "us-west-2".

`AWS_ACCESS_KEY_ID`: The AWS access key with permission to write to your S3 bucket. This may be omitted if using docker
secrets.

`AWS_SECRET_ACCESS_KEY`: The AWS secret key with permission to write to your S3 bucket. This may be omitted if using
docker secrets.

`S3_BUCKET`: The S3 bucket you'd like to use as your backup/restore bucket.

`S3_PATH`: You may specify a path within your bucket to use for backing up and restoring. Defaults to "/".

`DIRECTORY_PERMISSIONS`: Octal permissions for newly created directories (for example, "0755"). Defaults to 0755. _Files will be
automatically restored to the same permissions they had when they were backed up._

`CADENCE_HOURLY`: Cron schedule for running hourly backups. Defaults to "0 * * * *".

`CADENCE_DAILY`: Cron schedule for running daily backups. Defaults to "10 1 * * *".

`CADENCE_WEEKLY`: Cron schedule for running weekly backups. Defaults to "10 2 * * 0".

`CADENCE_MONTHLY`: Cron schedule for running monthly backups. Defaults to "10 3 1 * *".

`CHOWN_ENABLE`: Change ownership of restored files and their parent directories to `CHOWN_UID` / `CHOWN_GID`.
Unrelated files and the data directory itself are not recursively changed. Defaults to "false".

`CHOWN_UID`: The UID that will be used when changing the ownership of the files. Defaults to 1000.

`CHOWN_GID`: The GID that will be used when changing the group ownership of the files. Defaults to 1000.

`DATA_DIRECTORY`: The directory where the backup and restore will occur. Defaults to "/data".

`NUM_BACKUPS_HOURLY`: The number of hourly backups to keep. Can be disabled by setting to 0. Defaults to 3.

`NUM_BACKUPS_DAILY`: The number of daily backups to keep. Can be disabled by setting to 0. Defaults to 3.

`NUM_BACKUPS_WEEKLY`: The number of weekly backups to keep. Can be disabled by setting to 0. Defaults to 3.

`NUM_BACKUPS_MONTHLY`: The number of monthly backups to keep. Can be disabled by setting to 0. Defaults to 3.

`RESTORE_FILE`: Set this if you'd like to restore from a specific date. NOTE: This should exactly match the date folder
within the S3 bucket. I.E. "hourly/2019-09-21T19:35:32Z.tar.gz" the S3_PATH will be added automatically.

`RESTORE_FORCE`: You can force a restore by setting this to "true". This will ignore if the directory already has files
in it. Defaults to "false".

## Notes
Restore will update existing files but will not remove files that haven't been backed up before. Restore does not offer a directory-cleanup option.

## Cleanup
Backups are rotated with a configurable number of backups to keep.
  1. How many monthly backups would you like to keep? Defaults to 3.
  2. How many weekly backups would you like to keep? Defaults to 3.
  3. How many daily backups would you like to keep? Defaults to 3.
  4. How many hourly backups would you like to keep? Defaults to 3.

Set `NUM_BACKUPS_HOURLY=0` or `NUM_BACKUPS_DAILY=0` to disable those backups.
The legacy names `NUM_HOURLY_BACKUPS`, `NUM_DAILY_BACKUPS`, `NUM_WEEKLY_BACKUPS`, and
`NUM_MONTHLY_BACKUPS` remain accepted as fallbacks. Counts must be nonnegative integers.
Pruning and automatic restore consider only timestamped `.tar.gz` files in the four backup-type folders;
unrelated objects are ignored. Existing archive names remain supported.

## Logging
Logs are written to standard output. Failed direct backup and restore commands exit with a nonzero status.

## Examples
I built this for use in kubernetes, but I'd imagine you could use this in any orchestrator. For my example I use an init
container which only does a restore if the /data directory is completely empty. Then, a backup container which is
responsible for running the backup cron and backing up the files periodically.

```yaml
      initContainers:
        - name: s3-restore
          image: s3-backup-restore:local
          volumeMounts:
            - name: public-files
              mountPath: /data
          args: ["restore"]
          securityContext:
            runAsUser: 0 # Explicitly needed for CHOWN_ENABLE in this example.
          tty: true
          env:
            - name: AWS_ACCESS_KEY_ID
              valueFrom:
                secretKeyRef:
                  name: backup-keys
                  key: aws_access_key_id
            - name: AWS_SECRET_ACCESS_KEY
              valueFrom:
                secretKeyRef:
                  name: backup-keys
                  key: aws_secret_access_key
            - name: AWS_REGION
              value: "us-west-2"
            - name: S3_BUCKET
              value: "my-backup-bucket"
            - name: S3_PATH
              value: "my-backup-path"
            - name: CHOWN_ENABLE
              value: "true"
            - name: CHOWN_UID
              value: "1000"
            - name: CHOWN_GID
              value: "1000"
      containers:
        - name: s3-backup
          image: s3-backup-restore:local
          volumeMounts:
            - name: public-files
              mountPath: /data
          args: ["cron"]
          securityContext:
            runAsUser: 1000 # Must be able to read the mounted data.
            runAsGroup: 1000
          env:
            - name: AWS_ACCESS_KEY_ID
              valueFrom:
                secretKeyRef:
                  name: backup-keys
                  key: aws_access_key
            - name: AWS_SECRET_ACCESS_KEY
              valueFrom:
                secretKeyRef:
                  name: backup-keys
                  key: aws_secret_key
            - name: AWS_REGION
              value: "us-west-2"
            - name: S3_BUCKET
              value: "my-backup-bucket"
            - name: S3_PATH
              value: "my-backup-path"
            - name: CHOWN_ENABLE
              value: "true"
            - name: CHOWN_UID
              value: "1000"
            - name: CHOWN_GID
              value: "1000"
```
