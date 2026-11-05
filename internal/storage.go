package internal

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3API is the subset of S3 used for restoring and pruning backups.
type S3API interface {
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObjects(context.Context, *s3.DeleteObjectsInput, ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

type UploadAPI interface {
	UploadObject(context.Context, *transfermanager.UploadObjectInput, ...func(*transfermanager.Options)) (*transfermanager.UploadObjectOutput, error)
}

func backupPrefix(prefix string) string { return strings.TrimRight(prefix, "/") + "/" }

func validBackupType(kind string) bool {
	switch kind {
	case "hourly", "daily", "weekly", "monthly":
		return true
	}
	return false
}

// Only recognize our own timestamped archives, never unrelated objects sharing a prefix.
func backupTimestamp(key, prefix string) (time.Time, bool) {
	relative, ok := strings.CutPrefix(key, backupPrefix(prefix))
	if !ok {
		return time.Time{}, false
	}
	kind, name, ok := strings.Cut(relative, "/")
	if !ok || !validBackupType(kind) || !strings.HasSuffix(name, ".tar.gz") {
		return time.Time{}, false
	}
	timestamp, err := time.Parse(time.RFC3339Nano, strings.TrimSuffix(name, ".tar.gz"))
	return timestamp, err == nil
}

func listBackups(ctx context.Context, client S3API, bucket, prefix string) ([]types.Object, error) {
	pager := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)}, func(o *s3.ListObjectsV2PaginatorOptions) { o.StopOnDuplicateToken = true })
	var objects []types.Object
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list backups: %w", err)
		}
		objects = append(objects, page.Contents...)
	}
	return objects, nil
}
