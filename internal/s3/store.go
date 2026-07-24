package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/voxelum/xmcl-shared-node-agent/internal/objectstore"
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
	var data bytes.Buffer
	if _, err := s.DownloadTo(ctx, key, &data); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}

func (s *Store) DownloadTo(ctx context.Context, key string, destination io.Writer) (int64, error) {
	object, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return 0, fmt.Errorf("open S3 object: %w", err)
	}
	defer object.Close()
	size, err := io.Copy(destination, object)
	if err != nil {
		return size, fmt.Errorf("read S3 object: %w", err)
	}
	return size, nil
}

func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("stat S3 object: %w", err)
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

func (s *Store) UploadFile(ctx context.Context, key, path, contentType string) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open upload file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat upload file: %w", err)
	}
	result, err := s.client.PutObject(ctx, s.bucket, key, file, info.Size(), minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return 0, fmt.Errorf("upload S3 file: %w", err)
	}
	return result.Size, nil
}

func (s *Store) List(ctx context.Context, prefix string) ([]objectstore.ObjectInfo, error) {
	objects := make([]objectstore.ObjectInfo, 0)
	for object := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if object.Err != nil {
			return nil, fmt.Errorf("list S3 objects: %w", object.Err)
		}
		objects = append(objects, objectstore.ObjectInfo{Key: object.Key, LastModified: object.LastModified, Size: object.Size})
	}
	return objects, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete S3 object: %w", err)
	}
	return nil
}

func isNotFound(err error) bool {
	code := minio.ToErrorResponse(err).Code
	return code == "NoSuchKey" || code == "NoSuchObject" || strings.EqualFold(code, "NotFound")
}
