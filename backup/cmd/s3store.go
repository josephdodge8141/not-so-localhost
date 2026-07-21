package main

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type S3Store struct {
	client *s3.Client
	bucket string
}

func NewS3Store(client *s3.Client, bucket string) *S3Store {
	return &S3Store{client: client, bucket: bucket}
}

func (s *S3Store) Name() string {
	return fmt.Sprintf("s3://%s", s.bucket)
}

func (s *S3Store) Save(ctx context.Context, key string, r io.Reader) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   r,
	})
	return err
}

func (s *S3Store) Load(ctx context.Context, key string) (io.ReadCloser, error) {
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	return result.Body, nil
}

func (s *S3Store) DeletePrefix(ctx context.Context, prefix string) error {
	var keys []string
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, obj := range page.Contents {
			keys = append(keys, *obj.Key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	for i := 0; i < len(keys); i += 1000 {
		end := i + 1000
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[i:end]
		objects := make([]types.ObjectIdentifier, len(batch))
		for j, k := range batch {
			objects[j] = types.ObjectIdentifier{Key: aws.String(k)}
		}
		if _, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{
				Objects: objects,
				Quiet:   aws.Bool(true),
			},
		}); err != nil {
			return err
		}
	}
	return nil
}
