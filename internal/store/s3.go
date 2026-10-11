package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3 is a store in an S3-compatible bucket. It uses the AWS SDK's standard
// configuration: credentials from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY
// (or any other SDK source), AWS_REGION (default "auto"), and
// AWS_ENDPOINT_URL_S3 for non-AWS stores such as R2 or MinIO. Set
// SESH_S3_PATH_STYLE=1 for stores that need path-style addressing.
type S3 struct {
	Client *s3.Client
	Bucket string
	Prefix string // key prefix within the bucket, without leading or trailing slash
}

// NewS3 returns an S3 store configured from the environment.
func NewS3(bucket, prefix string) (*S3, error) {
	opts := []func(*config.LoadOptions) error{}
	if os.Getenv("AWS_REGION") == "" && os.Getenv("AWS_DEFAULT_REGION") == "" {
		opts = append(opts, config.WithRegion("auto"))
	}
	cfg, err := config.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = os.Getenv("SESH_S3_PATH_STYLE") != ""
	})
	return &S3{Client: client, Bucket: bucket, Prefix: strings.Trim(prefix, "/")}, nil
}

func (s *S3) key(name string) string {
	if s.Prefix == "" {
		return "sesh/" + name
	}
	return s.Prefix + "/sesh/" + name
}

func (s *S3) Put(ctx context.Context, name string, data []byte) error {
	if err := CheckName(name); err != nil {
		return err
	}
	_, err := s.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.Bucket),
		Key:         aws.String(s.key(name)),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/octet-stream"),
	})
	return err
}

func (s *S3) Get(ctx context.Context, name string) ([]byte, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	out, err := s.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(s.key(name)),
	})
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	if err := checkPrefix(prefix); err != nil {
		return nil, err
	}
	base := s.key("")
	p := s3.NewListObjectsV2Paginator(s.Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.Bucket),
		Prefix: aws.String(base + prefix),
	})
	var names []string
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			name := strings.TrimPrefix(aws.ToString(o.Key), base)
			if CheckName(name) == nil {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names, nil
}
