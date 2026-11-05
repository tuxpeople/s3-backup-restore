package internal

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"os"
	"path/filepath"
	"testing"
)

type fakeUpload func(context.Context, *transfermanager.UploadObjectInput) (*transfermanager.UploadObjectOutput, error)

func (f fakeUpload) UploadObject(ctx context.Context, in *transfermanager.UploadObjectInput, _ ...func(*transfermanager.Options)) (*transfermanager.UploadObjectOutput, error) {
	return f(ctx, in)
}

func TestBackupFailureNeverPrunesAndCleansTemporaryFiles(t *testing.T) {
	for _, compressionFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "upload failure", true: "compression failure"}[compressionFailure], func(t *testing.T) {
			temporary := t.TempDir()
			t.Setenv("TMPDIR", temporary)
			data := t.TempDir()
			if compressionFailure {
				if err := os.Symlink("/outside", filepath.Join(data, "link")); err != nil {
					t.Fatal(err)
				}
			}
			uploads, lists, deletes := 0, 0, 0
			backup := Backup{DataDirectory: data, HourlyBackups: 3, S3Bucket: "bucket", S3Path: "prefix",
				UploadClient: fakeUpload(func(_ context.Context, in *transfermanager.UploadObjectInput) (*transfermanager.UploadObjectOutput, error) {
					uploads++
					return nil, errors.New("upload failed")
				}),
				S3Service: fakeS3{list: func(*s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
					lists++
					return &s3.ListObjectsV2Output{}, nil
				}, delete: func(*s3.DeleteObjectsInput) (*s3.DeleteObjectsOutput, error) {
					deletes++
					return &s3.DeleteObjectsOutput{}, nil
				}},
			}
			if err := backup.Run(context.Background(), "hourly"); err == nil {
				t.Fatal("backup failure ignored")
			}
			if lists != 0 || deletes != 0 {
				t.Fatalf("pruning attempted after failure: lists=%d deletes=%d", lists, deletes)
			}
			if compressionFailure && uploads != 0 {
				t.Fatal("uploaded an incomplete archive")
			}
			// t.TempDir above also lives below TMPDIR; only match this application's files.
			leftovers, err := filepath.Glob(filepath.Join(temporary, "s3-backup-*.tar.gz"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("temporary archive leaked: %v %v", leftovers, err)
			}
		})
	}
}

func TestBackupRejectsOverlapAndKeepsActiveArchive(t *testing.T) {
	data := t.TempDir()
	entered := make(chan string)
	release := make(chan struct{})
	done := make(chan error, 1)
	backup := Backup{DataDirectory: data, HourlyBackups: 3, DailyBackups: 3, S3Bucket: "bucket", S3Path: "prefix",
		UploadClient: fakeUpload(func(_ context.Context, in *transfermanager.UploadObjectInput) (*transfermanager.UploadObjectOutput, error) {
			entered <- in.Body.(*os.File).Name()
			<-release
			return &transfermanager.UploadObjectOutput{}, nil
		}),
		S3Service: fakeS3{list: func(*s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) { return &s3.ListObjectsV2Output{}, nil }},
	}
	go func() { done <- backup.Run(context.Background(), "hourly") }()
	archive := <-entered
	func() {
		defer close(release)
		if err := backup.Run(context.Background(), "daily"); err == nil {
			t.Error("overlapping backup accepted")
		}
		if _, err := os.Stat(archive); err != nil {
			t.Error("active archive was removed", err)
		}
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(archive); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive not cleaned: %v", err)
	}
}
