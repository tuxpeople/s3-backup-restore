package internal

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

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

func archiveName(name string) (string, error) {
	if !filepath.IsLocal(name) || strings.Contains(name, "\\") {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("unsafe archive path %q", name)
		}
	}
	return path.Clean(name), nil
}

func (r Restore) restoreBackup(archive string) (err error) {
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
	root, err := os.OpenRoot(r.DataDirectory)
	if err != nil {
		return err
	}
	defer root.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name, err := archiveName(header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := r.makeDirectories(root, name); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if name == "." {
				return errors.New("archive file cannot name the data directory")
			}
			if err := r.makeDirectories(root, path.Dir(name)); err != nil {
				return err
			}
			if err := r.restoreFile(root, name, header, reader); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry %q (type %d); links and special files are forbidden", name, header.Typeflag)
		}
	}
	// tar's end marker can occur before gzip's checksum; verify the entire stream.
	_, err = io.Copy(io.Discard, gz)
	return err
}

func (r Restore) makeDirectories(root *os.Root, name string) error {
	if name == "." {
		return nil
	}
	current := ""
	for _, part := range strings.Split(name, "/") {
		current = path.Join(current, part)
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := root.Mkdir(current, r.NewDirectoryPermissions.Perm()); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if !info.IsDir() {
			return fmt.Errorf("restore path is not a directory or is a symlink: %q", current)
		}
		if r.ChownEnable {
			// Use the opened directory, never a path-following recursive chown.
			dir, err := root.Open(current)
			if err != nil {
				return err
			}
			stat, statErr := dir.Stat()
			if statErr != nil {
				dir.Close()
				return statErr
			}
			if !stat.IsDir() {
				dir.Close()
				return fmt.Errorf("directory changed during restore: %q", current)
			}
			err = errors.Join(dir.Chown(r.ChownUID, r.ChownGID), dir.Close())
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (r Restore) restoreFile(root *os.Root, name string, header *tar.Header, reader io.Reader) (err error) {
	info, err := root.Lstat(name)
	if err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to overwrite symlink or special file %q", name)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Atomic replacement avoids writing through hardlinks and leaves an existing
	// file intact if copying, chmod, chown, or closing the replacement fails.
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	temporary := path.Join(path.Dir(name), ".s3-restore-"+hex.EncodeToString(random))
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	defer func() {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
	}()
	if _, err := io.CopyN(file, reader, header.Size); err != nil {
		return err
	}
	if r.ChownEnable {
		if err := file.Chown(r.ChownUID, r.ChownGID); err != nil {
			return err
		}
	}
	if err := file.Chmod(header.FileInfo().Mode().Perm()); err != nil {
		return err
	}
	err = file.Close()
	file = nil
	if err != nil {
		return err
	}
	return root.Rename(temporary, name)
}
