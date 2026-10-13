package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3 is a store in an S3-compatible bucket.
type S3 struct {
	Client *s3.Client
	Bucket string
	Prefix string // key prefix within the bucket, without leading or trailing slash
}

// S3Config is an S3 store's credentials and endpoint. sesh fills it from its
// own settings only, never from the AWS_ variables or shared config files
// other AWS tools read.
type S3Config struct {
	AccessKeyID, SecretAccessKey string // required
	SessionToken                 string // for temporary credentials
	Region                       string // default "auto"
	Endpoint                     string // for stores other than AWS, such as R2 or MinIO
	PathStyle                    bool   // for stores that need path-style addressing
}

// NewS3 returns an S3 store for a bucket and key prefix.
func NewS3(bucket, prefix string, c S3Config) (*S3, error) {
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return nil, errors.New("an s3 store needs credentials: run sesh init -aws, or set SESH_AWS_ACCESS_KEY_ID and SESH_AWS_SECRET_ACCESS_KEY")
	}
	o := s3.Options{
		Region:       "auto",
		Credentials:  credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, c.SessionToken),
		UsePathStyle: c.PathStyle,
	}
	if c.Region != "" {
		o.Region = c.Region
	}
	if c.Endpoint != "" {
		o.BaseEndpoint = aws.String(c.Endpoint)
	}
	return &S3{Client: s3.New(o), Bucket: bucket, Prefix: strings.Trim(prefix, "/")}, nil
}

func (s *S3) key(name string) string {
	if s.Prefix == "" {
		return name
	}
	return s.Prefix + "/" + name
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
