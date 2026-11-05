package internal

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Serialize all backup cadences within this process, including pruning.
var backupMu sync.Mutex

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
	if !backupMu.TryLock() {
		return errors.New("another backup is already running; skipping overlapping job")
	}
	defer backupMu.Unlock()
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
	root, err := os.OpenRoot(b.DataDirectory)
	if err != nil {
		return "", err
	}
	defer root.Close()
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
	// Close in this order and propagate every error before allowing an upload.
	defer func() { err = errors.Join(err, tw.Close(), gz.Close()) }()
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("refusing symlink or special file %q", path)
		}
		if path == "BACKUP_DATE" {
			if info.IsDir() {
				return fmt.Errorf("BACKUP_DATE must be a regular file")
			}
			return nil
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = path
		if info.IsDir() {
			return tw.WriteHeader(header)
		}
		return addRegularFile(root, tw, path, info, header)
	})
	if err != nil {
		return name, err
	}
	// Keep the legacy marker inside the archive without writing through a link in /data.
	marker := []byte(fmt.Sprintf("%s/%s\n", kind, timestamp))
	if err = tw.WriteHeader(&tar.Header{Name: "BACKUP_DATE", Mode: 0644, Size: int64(len(marker)), Typeflag: tar.TypeReg}); err != nil {
		return name, err
	}
	_, err = tw.Write(marker)
	return name, err
}

func addRegularFile(root *os.Root, tw *tar.Writer, name string, expected os.FileInfo, header *tar.Header) error {
	// Root prevents escapes even if a directory is exchanged for a symlink during the walk.
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil {
		return err
	}
	if !actual.Mode().IsRegular() || !os.SameFile(expected, actual) {
		return fmt.Errorf("file changed during backup: %q", name)
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	if _, err := io.Copy(tw, file); err != nil {
		return err
	}
	after, err := file.Stat()
	if err != nil {
		return err
	}
	if after.Size() != actual.Size() || !after.ModTime().Equal(actual.ModTime()) {
		return fmt.Errorf("file changed during backup: %q", name)
	}
	return nil
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
