package internal

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"io"
	"os"
	"strings"
	"testing"
)

type fakeS3 struct {
	S3API
	list   func(*s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error)
	delete func(*s3.DeleteObjectsInput) (*s3.DeleteObjectsOutput, error)
	get    func(*s3.GetObjectInput) (*s3.GetObjectOutput, error)
}

func (f fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return f.list(in)
}
func (f fakeS3) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	return f.delete(in)
}
func (f fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return f.get(in)
}

func TestPruningPaginatesAndOnlyDeletesOwnedArchives(t *testing.T) {
	calls, deletes := 0, 0
	old := "prefix/hourly/2020-01-01T00:00:00Z.tar.gz"
	client := fakeS3{
		list: func(in *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
			calls++
			if aws.ToString(in.Prefix) != "prefix/hourly/" {
				t.Fatalf("unsafe prefix %q", aws.ToString(in.Prefix))
			}
			if calls == 1 {
				return &s3.ListObjectsV2Output{Contents: []types.Object{{Key: aws.String(old)}, {Key: aws.String("prefix/hourly/keep-me.txt")}}, IsTruncated: aws.Bool(true), NextContinuationToken: aws.String("next")}, nil
			}
			if aws.ToString(in.ContinuationToken) != "next" {
				t.Fatal("pagination token missing")
			}
			return &s3.ListObjectsV2Output{Contents: []types.Object{{Key: aws.String("prefix/hourly/2026-01-01T00:00:00Z.tar.gz")}}}, nil
		},
		delete: func(in *s3.DeleteObjectsInput) (*s3.DeleteObjectsOutput, error) {
			deletes++
			if len(in.Delete.Objects) != 1 || aws.ToString(in.Delete.Objects[0].Key) != old {
				t.Fatalf("wrong objects pruned: %+v", in.Delete.Objects)
			}
			return &s3.DeleteObjectsOutput{}, nil
		},
	}
	if err := (Backup{S3Service: client, S3Bucket: "bucket", S3Path: "prefix"}).pruneS3(context.Background(), "hourly", 1); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || deletes != 1 {
		t.Fatalf("unexpected calls: list=%d delete=%d", calls, deletes)
	}
}

func TestPruningReportsPerObjectErrors(t *testing.T) {
	client := fakeS3{
		list: func(*s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
			return &s3.ListObjectsV2Output{Contents: []types.Object{{Key: aws.String("p/hourly/2020-01-01T00:00:00Z.tar.gz")}, {Key: aws.String("p/hourly/2026-01-01T00:00:00Z.tar.gz")}}}, nil
		},
		delete: func(*s3.DeleteObjectsInput) (*s3.DeleteObjectsOutput, error) {
			return &s3.DeleteObjectsOutput{Errors: []types.Error{{Code: aws.String("AccessDenied")}}}, nil
		},
	}
	if err := (Backup{S3Service: client, S3Path: "p"}).pruneS3(context.Background(), "hourly", 1); err == nil {
		t.Fatal("per-object S3 error ignored")
	}
}

func TestLatestBackupPaginatesAndIgnoresUnrelatedKeys(t *testing.T) {
	calls := 0
	expected := "prefix/daily/2026-01-01T00:00:00Z.tar.gz"
	client := fakeS3{list: func(in *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
		calls++
		if aws.ToString(in.Prefix) != "prefix/" {
			t.Fatal("missing prefix boundary")
		}
		if calls == 1 {
			return &s3.ListObjectsV2Output{Contents: []types.Object{{Key: aws.String("prefix/hourly/2020-01-01T00:00:00Z.tar.gz")}, {Key: aws.String("prefix/unrelated/2099-01-01T00:00:00Z.tar.gz")}, {Key: aws.String("prefix/notes.txt")}}, IsTruncated: aws.Bool(true), NextContinuationToken: aws.String("next")}, nil
		}
		return &s3.ListObjectsV2Output{Contents: []types.Object{{Key: aws.String(expected)}}}, nil
	}}
	key, err := (Restore{S3Service: client, S3Path: "prefix"}).getLatestBackup(context.Background())
	if err != nil || key != expected || calls != 2 {
		t.Fatalf("wrong backup: %q %v calls=%d", key, err, calls)
	}
}

type failingCloseBody struct{ io.Reader }

func (failingCloseBody) Close() error { return errors.New("body close failed") }

func TestDownloadFailureRemovesTemporaryFile(t *testing.T) {
	temporary := t.TempDir()
	t.Setenv("TMPDIR", temporary)
	client := fakeS3{get: func(*s3.GetObjectInput) (*s3.GetObjectOutput, error) {
		return &s3.GetObjectOutput{Body: failingCloseBody{strings.NewReader("data")}}, nil
	}}
	name, err := (Restore{S3Service: client}).downloadBackup(context.Background(), "key")
	if err == nil || name != "" {
		t.Fatalf("failed download returned %q %v", name, err)
	}
	entries, err := os.ReadDir(temporary)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary download leaked: %v %v", entries, err)
	}
}

func TestInvalidRetentionDoesNotAccessS3(t *testing.T) {
	for _, test := range []struct {
		kind  string
		count int
	}{{"hourly", -1}, {"unknown", 3}} {
		if err := (Backup{HourlyBackups: test.count}).Run(context.Background(), test.kind); err == nil {
			t.Fatalf("invalid retention accepted: %+v", test)
		}
	}
	if err := (Backup{HourlyBackups: 0}).Run(context.Background(), "hourly"); err != nil {
		t.Fatal(err)
	}
}
