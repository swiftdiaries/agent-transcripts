package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// NewAWSS3 constructs the production S3 API using the normal AWS credential
// chain. endpoint is optional and enables S3-compatible services.
func NewAWSS3(ctx context.Context, region, endpoint string) (S3API, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if endpoint != "" {
		opts = append(opts, awsconfig.WithBaseEndpoint(endpoint))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &awsS3{client: awss3.NewFromConfig(cfg, func(o *awss3.Options) { o.UsePathStyle = endpoint != "" })}, nil
}

type awsS3 struct{ client *awss3.Client }

func (a *awsS3) GetObject(ctx context.Context, bucket, key string) (S3Object, error) {
	out, err := a.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return S3Object{}, mapAWSError(err)
	}
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		return S3Object{}, err
	}
	return S3Object{Body: body, ETag: strings.Trim(aws.ToString(out.ETag), "\"")}, nil
}
func (a *awsS3) HeadObject(ctx context.Context, bucket, key string) (S3Object, error) {
	out, err := a.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return S3Object{}, mapAWSError(err)
	}
	return S3Object{ETag: strings.Trim(aws.ToString(out.ETag), "\"")}, nil
}
func (a *awsS3) PutObject(ctx context.Context, bucket, key string, body []byte, c S3Condition) (string, error) {
	in := &awss3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(body)}
	if c.IfNoneMatch {
		in.IfNoneMatch = aws.String("*")
	}
	if c.IfMatch != "" {
		in.IfMatch = aws.String(c.IfMatch)
	}
	out, err := a.client.PutObject(ctx, in)
	if err != nil {
		return "", mapAWSError(err)
	}
	return strings.Trim(aws.ToString(out.ETag), "\""), nil
}
func (a *awsS3) CopyObject(ctx context.Context, bucket, src, dst string, c S3Condition) (string, error) {
	// S3 CopyObject has no destination If-Match/If-None-Match precondition.
	// Read then conditionally put preserves this store's atomic destination
	// semantics; copied transcript files are immutable after publication.
	source, err := a.GetObject(ctx, bucket, src)
	if err != nil {
		return "", err
	}
	return a.PutObject(ctx, bucket, dst, source.Body, c)
}
func (a *awsS3) DeleteObject(ctx context.Context, bucket, key string, c S3Condition) error {
	in := &awss3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if c.IfMatch != "" {
		in.IfMatch = aws.String(c.IfMatch)
	}
	_, err := a.client.DeleteObject(ctx, in)
	return mapAWSError(err)
}
func (a *awsS3) ListObjectsV2(ctx context.Context, bucket, prefix, token string) (S3ListPage, error) {
	out, err := a.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix), ContinuationToken: aws.String(token)})
	if err != nil {
		return S3ListPage{}, mapAWSError(err)
	}
	page := S3ListPage{NextToken: aws.ToString(out.NextContinuationToken)}
	for _, o := range out.Contents {
		page.Keys = append(page.Keys, aws.ToString(o.Key))
	}
	return page, nil
}

func mapAWSError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket":
			return ErrS3NotFound
		case "PreconditionFailed", "ConditionalRequestConflict":
			return ErrS3PreconditionFailed
		}
	}
	return err
}
