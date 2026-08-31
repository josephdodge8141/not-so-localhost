// Package main runs the distributed Not-So-Localhost app registry.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// S3Config selects the registry bucket and AWS region.
type S3Config struct {
	Bucket string
	Region string
}

type s3API interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// S3ObjectStore persists registry objects in S3.
type S3ObjectStore struct {
	client s3API
	bucket string
}

// NewS3ObjectStore creates an S3-backed registry object store.
func NewS3ObjectStore(ctx context.Context, cfg S3Config) (*S3ObjectStore, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("REGISTRY_S3_BUCKET is required")
	}
	awsConfig, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.Region))
	if err != nil {
		return nil, err
	}
	return &S3ObjectStore{client: s3.NewFromConfig(awsConfig), bucket: cfg.Bucket}, nil
}

// Get retrieves an object and its S3 ETag.
func (s *S3ObjectStore) Get(ctx context.Context, key string) (StoredObject, error) {
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return StoredObject{}, mapS3Error(err)
	}
	defer result.Body.Close()
	body, err := io.ReadAll(io.LimitReader(result.Body, 16<<20))
	if err != nil {
		return StoredObject{}, err
	}
	return StoredObject{Body: body, ETag: aws.ToString(result.ETag)}, nil
}

// Put performs a conditional S3 object write.
func (s *S3ObjectStore) Put(ctx context.Context, key string, body []byte, options PutOptions) (string, error) {
	input := &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String(options.ContentType),
	}
	if options.IfMatch != "" {
		input.IfMatch = aws.String(options.IfMatch)
	}
	if options.IfNoneMatch {
		input.IfNoneMatch = aws.String("*")
	}
	result, err := s.client.PutObject(ctx, input)
	if err != nil {
		return "", mapS3Error(err)
	}
	return aws.ToString(result.ETag), nil
}

func mapS3Error(err error) error {
	var responseError *smithyhttp.ResponseError
	if errors.As(err, &responseError) {
		switch responseError.HTTPStatusCode() {
		case 404:
			return ErrNotFound
		case 409, 412:
			return ErrPrecondition
		}
	}
	return err
}
