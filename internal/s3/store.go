package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Store struct {
	client *minio.Client
	bucket string
}

func New(endpoint, region, bucket, accessKey, secretKey string) (*Store, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: true,
		Region: region,
	})
	if err != nil {
		return nil, fmt.Errorf("create S3 client: %w", err)
	}
	return &Store{client: client, bucket: bucket}, nil
}

func (s *Store) Validate(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("check S3 bucket: %w", err)
	}
	if !exists {
		return fmt.Errorf("S3 bucket %q does not exist or is not accessible", s.bucket)
	}
	return nil
}

func (s *Store) Download(ctx context.Context, key string) ([]byte, error) {
	object, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("open S3 object: %w", err)
	}
	defer object.Close()
	data, err := io.ReadAll(object)
	if err != nil {
		return nil, fmt.Errorf("read S3 object: %w", err)
	}
	return data, nil
}

func (s *Store) Upload(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return fmt.Errorf("upload S3 object: %w", err)
	}
	return nil
}
