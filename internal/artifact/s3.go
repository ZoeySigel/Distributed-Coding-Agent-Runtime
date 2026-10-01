package artifact

import (
	"bytes"
	"context"
	"fmt"
	"github.com/dcar/runtime/internal/domain"
	"github.com/dcar/runtime/internal/store"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"io"
	"time"
)

const MaxSize = 32 << 20

type ArtifactStore interface {
	Put(context.Context, string, string, []byte) (domain.Artifact, error)
	Verify(context.Context, domain.Artifact) error
	Get(context.Context, string) (io.ReadCloser, error)
}
type S3 struct {
	Client *minio.Client
	Bucket string
}

func New(ctx context.Context, endpoint, key, secret, bucket string, secure bool) (*S3, error) {
	c, e := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(key, secret, ""), Secure: secure})
	if e != nil {
		return nil, e
	}
	ok, e := c.BucketExists(ctx, bucket)
	if e != nil {
		return nil, e
	}
	if !ok {
		if e = c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); e != nil {
			ok, re := c.BucketExists(ctx, bucket)
			if re != nil || !ok {
				return nil, e
			}
		}
	}
	return &S3{c, bucket}, nil
}
func (s *S3) Put(ctx context.Context, attempt, name string, b []byte) (domain.Artifact, error) {
	a := domain.Artifact{Name: name, SHA256: store.Hash(b), Size: int64(len(b))}
	a.Key = attempt + "/" + a.SHA256 + "/" + name
	if len(b) > MaxSize {
		return a, fmt.Errorf("artifact exceeds size limit")
	}
	_, e := s.Client.PutObject(ctx, s.Bucket, a.Key, bytes.NewReader(b), a.Size, minio.PutObjectOptions{ContentType: "application/octet-stream", UserMetadata: map[string]string{"sha256": a.SHA256}})
	return a, e
}
func (s *S3) Verify(ctx context.Context, a domain.Artifact) error {
	i, e := s.Client.StatObject(ctx, s.Bucket, a.Key, minio.StatObjectOptions{})
	if e != nil {
		return e
	}
	if i.Size != a.Size || i.UserMetadata["Sha256"] != a.SHA256 { // Header capitalization differs across S3 implementations.
		if i.Size != a.Size || i.Metadata.Get("X-Amz-Meta-Sha256") != a.SHA256 {
			return fmt.Errorf("artifact checksum metadata mismatch")
		}
	}
	return nil
}
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	o, e := s.Client.GetObject(ctx, s.Bucket, key, minio.GetObjectOptions{})
	if e != nil {
		return nil, e
	}
	if _, e = o.Stat(); e != nil {
		o.Close()
		return nil, e
	}
	return o, nil
}
func (s *S3) Sweep(ctx context.Context, db *store.Store) error {
	conn, e := db.Pool.Acquire(ctx)
	if e != nil {
		return e
	}
	defer conn.Release()
	var locked bool
	if e = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(84123872)").Scan(&locked); e != nil {
		return e
	}
	if !locked {
		return nil
	}
	defer func() {
		unlock, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_, _ = conn.Exec(unlock, "SELECT pg_advisory_unlock(84123872)")
	}()
	// Grace exceeds maximum task duration. Active attempts are never collected.
	for o := range s.Client.ListObjects(ctx, s.Bucket, minio.ListObjectsOptions{Recursive: true}) {
		if o.Err != nil {
			return o.Err
		}
		if time.Since(o.LastModified) < 48*time.Hour {
			continue
		}
		var keep bool
		e := db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM artifacts WHERE key=$1 AND created_at>clock_timestamp()-interval '7 days') OR EXISTS(SELECT 1 FROM attempts WHERE id=split_part($1,'/',1) AND status='running')`, o.Key).Scan(&keep)
		if e != nil {
			return e
		}
		if !keep {
			if e = s.Client.RemoveObject(ctx, s.Bucket, o.Key, minio.RemoveObjectOptions{}); e != nil {
				return e
			}
		}
	}
	_, e = db.Pool.Exec(ctx, "DELETE FROM artifacts WHERE created_at<clock_timestamp()-interval '7 days'")
	if e != nil {
		return e
	}
	_, e = db.Pool.Exec(ctx, "DELETE FROM tasks WHERE status IN ('succeeded','failed','cancelled','timed_out') AND updated_at<clock_timestamp()-interval '30 days'")
	return e
}
