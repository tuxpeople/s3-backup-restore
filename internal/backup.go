package internal

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type Backup struct {
	HourlyBackups, DailyBackups, WeeklyBackups, MonthlyBackups int
	S3Bucket, S3Path, DataDirectory                            string
	S3Service                                                  S3API
	UploadClient                                               UploadAPI
}

func (b Backup) retention(kind string) (int, error) {
	var count int
	switch kind {
	case "hourly":
		count = b.HourlyBackups
	case "daily":
		count = b.DailyBackups
	case "weekly":
		count = b.WeeklyBackups
	case "monthly":
		count = b.MonthlyBackups
	default:
		return 0, fmt.Errorf("invalid backup type %q", kind)
	}
	if count < 0 {
		return 0, errors.New("backup retention cannot be negative")
	}
	return count, nil
}

func (b Backup) Run(ctx context.Context, kind string) error {
	count, err := b.retention(kind)
	if err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	archive, err := b.compressDirectory(timestamp, kind)
	if err != nil {
		return fmt.Errorf("create backup: %w", err)
	}
	defer os.Remove(archive)
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	key := backupPrefix(b.S3Path) + kind + "/" + timestamp + ".tar.gz"
	if _, err := b.UploadClient.UploadObject(ctx, &transfermanager.UploadObjectInput{Bucket: aws.String(b.S3Bucket), Key: aws.String(key), Body: file}); err != nil {
		return fmt.Errorf("upload backup: %w", err)
	}
	// Never prune after a compression, finalization, or upload failure.
	if err := b.pruneS3(ctx, kind, count); err != nil {
		return err
	}
	slog.Info("Backup complete", "key", key)
	return nil
}

func (b Backup) compressDirectory(timestamp, kind string) (name string, err error) {
	if err := os.WriteFile(filepath.Join(b.DataDirectory, "BACKUP_DATE"), []byte(fmt.Sprintf("%s/%s\n", kind, timestamp)), 0644); err != nil {
		return "", err
	}
	file, err := os.CreateTemp("", "s3-backup-*.tar.gz")
	if err != nil {
		return "", err
	}
	name = file.Name()
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			os.Remove(name)
			name = ""
		}
	}()
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	defer func() { err = errors.Join(err, tw.Close(), gz.Close()) }()
	err = filepath.Walk(b.DataDirectory, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		return b.addFile(tw, path)
	})
	return name, err
}

func (b Backup) addFile(tw *tar.Writer, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	header, err := tar.FileInfoHeader(info, path)
	if err != nil {
		return err
	}
	header.Name = strings.ReplaceAll(path, b.DataDirectory+"/", "")
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err = io.Copy(tw, file)
	return err
}

func (b Backup) pruneS3(ctx context.Context, kind string, count int) error {
	objects, err := listBackups(ctx, b.S3Service, b.S3Bucket, backupPrefix(b.S3Path)+kind+"/")
	if err != nil {
		return err
	}
	type backup struct {
		key       string
		timestamp time.Time
	}
	var backups []backup
	for _, object := range objects {
		key := aws.ToString(object.Key)
		if timestamp, ok := backupTimestamp(key, b.S3Path); ok {
			backups = append(backups, backup{key, timestamp})
		}
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].timestamp.After(backups[j].timestamp) })
	if len(backups) <= count {
		return nil
	}
	backups = backups[count:]
	for len(backups) > 0 {
		size := min(len(backups), 1000)
		batch := make([]types.ObjectIdentifier, size)
		for i := range batch {
			batch[i].Key = aws.String(backups[i].key)
		}
		result, err := b.S3Service.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(b.S3Bucket), Delete: &types.Delete{Objects: batch, Quiet: aws.Bool(true)}})
		if err != nil {
			return fmt.Errorf("prune backups: %w", err)
		}
		if len(result.Errors) > 0 {
			return fmt.Errorf("prune backups failed for %q: %s", aws.ToString(result.Errors[0].Key), aws.ToString(result.Errors[0].Code))
		}
		backups = backups[size:]
	}
	return nil
}
