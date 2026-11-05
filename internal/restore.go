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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type Restore struct {
	S3Bucket, S3Path, DataDirectory string
	NewDirectoryPermissions         os.FileMode
	ChownEnable                     bool
	ChownUID, ChownGID              int
	ForceRestore                    bool
	RestoreFile                     string
	S3Service                       S3API
}

func (r Restore) Run(ctx context.Context) error {
	entries, err := os.ReadDir(r.DataDirectory)
	if err != nil {
		return fmt.Errorf("read data directory: %w", err)
	}
	if !r.ForceRestore && len(entries) > 0 {
		slog.Info("Skipping restore: data directory is not empty")
		return nil
	}
	key := backupPrefix(r.S3Path) + r.RestoreFile
	if r.RestoreFile == "" {
		key, err = r.getLatestBackup(ctx)
		if err != nil {
			return err
		}
	} else if _, ok := backupTimestamp(key, r.S3Path); !ok {
		return fmt.Errorf("invalid backup filename %q", r.RestoreFile)
	}
	archive, err := r.downloadBackup(ctx, key)
	if err != nil {
		return err
	}
	defer os.Remove(archive)
	if err := r.restoreBackup(archive); err != nil {
		return fmt.Errorf("restore backup: %w", err)
	}
	slog.Info("Backup restored", "key", key)
	return nil
}

func (r Restore) getLatestBackup(ctx context.Context) (string, error) {
	objects, err := listBackups(ctx, r.S3Service, r.S3Bucket, backupPrefix(r.S3Path))
	if err != nil {
		return "", err
	}
	var latest string
	for _, object := range objects {
		key := aws.ToString(object.Key)
		timestamp, ok := backupTimestamp(key, r.S3Path)
		if !ok {
			continue
		}
		previous, _ := backupTimestamp(latest, r.S3Path)
		if latest == "" || timestamp.After(previous) {
			latest = key
		}
	}
	if latest == "" {
		return "", errors.New("unable to find a backup to restore from")
	}
	return latest, nil
}

func (r Restore) downloadBackup(ctx context.Context, key string) (name string, err error) {
	result, err := r.S3Service.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.S3Bucket), Key: aws.String(key)})
	if err != nil {
		return "", fmt.Errorf("download backup: %w", err)
	}
	defer func() {
		err = errors.Join(err, result.Body.Close())
		if err != nil && name != "" {
			os.Remove(name)
			name = ""
		}
	}()
	file, err := os.CreateTemp("", "s3-restore-*.tar.gz")
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
	_, err = io.Copy(file, result.Body)
	return name, err
}

func (r Restore) restoreBackup(archive string) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := fmt.Sprintf("%s/%s", r.DataDirectory, header.Name)
		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(name), r.NewDirectoryPermissions); err != nil {
				return err
			}
			output, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, header.FileInfo().Mode().Perm())
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(output, reader)
			if err := errors.Join(copyErr, output.Close()); err != nil {
				return err
			}
		default:
			slog.Error("Unsupported archive entry", "name", name, "type", header.Typeflag)
		}
	}
	if r.ChownEnable {
		return filepath.Walk(r.DataDirectory, func(name string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			return os.Chown(name, r.ChownUID, r.ChownGID)
		})
	}
	return nil
}
