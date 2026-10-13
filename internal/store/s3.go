package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3 is a store in an S3-compatible bucket.
type S3 struct {
	Client *s3.Client
	Bucket string
	Prefix string // key prefix within the bucket, without leading or trailing slash
	Owner  string // if set, the AWS account that must own the bucket
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
	AccountID                    string // if set, the AWS account that must own the bucket
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
	return &S3{Client: s3.New(o), Bucket: bucket, Prefix: strings.Trim(prefix, "/"), Owner: c.AccountID}, nil
}

func (s *S3) owner() *string {
	if s.Owner == "" {
		return nil
	}
	return aws.String(s.Owner)
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
		Bucket:              aws.String(s.Bucket),
		Key:                 aws.String(s.key(name)),
		Body:                bytes.NewReader(data),
		ContentType:         aws.String("application/octet-stream"),
		ExpectedBucketOwner: s.owner(),
	})
	return err
}

func (s *S3) Get(ctx context.Context, name string) ([]byte, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	out, err := s.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket:              aws.String(s.Bucket),
		Key:                 aws.String(s.key(name)),
		ExpectedBucketOwner: s.owner(),
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
		Bucket:              aws.String(s.Bucket),
		Prefix:              aws.String(base + prefix),
		ExpectedBucketOwner: s.owner(),
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

// CheckPrivate returns an error unless the bucket blocks public access with
// all four of its Block Public Access settings, which together keep both
// ACLs and bucket policies from making anything in it public. sesh refuses
// a bucket that doesn't: its objects are encrypted, but their names, sizes
// and times would show, and anyone could add or overwrite them.
func (s *S3) CheckPrivate(ctx context.Context) error {
	return CheckPrivate(ctx, s.Client, s.Bucket, s.Owner)
}

// CheckPrivate is S3.CheckPrivate for any client of the bucket.
func CheckPrivate(ctx context.Context, c *s3.Client, bucket, owner string) error {
	in := &s3.GetPublicAccessBlockInput{Bucket: aws.String(bucket)}
	if owner != "" {
		in.ExpectedBucketOwner = aws.String(owner)
	}
	out, err := c.GetPublicAccessBlock(ctx, in)
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchPublicAccessBlockConfiguration":
			return notPrivate(bucket, "it has no Block Public Access settings")
		case "AccessDenied":
			msg := fmt.Sprintf("can't check that bucket %s blocks public access: access denied. "+
				"The credentials need s3:GetBucketPublicAccessBlock on it, which sesh init -aws grants", bucket)
			if owner != "" {
				msg += fmt.Sprintf(", and the bucket must belong to AWS account %s", owner)
			}
			return errors.New(msg)
		}
	}
	if err != nil {
		return fmt.Errorf("checking that bucket %s blocks public access: %w", bucket, err)
	}
	b := out.PublicAccessBlockConfiguration
	if b == nil {
		return notPrivate(bucket, "it has no Block Public Access settings")
	}
	var off []string
	for _, f := range []struct {
		name string
		on   *bool
	}{
		{"BlockPublicAcls", b.BlockPublicAcls},
		{"IgnorePublicAcls", b.IgnorePublicAcls},
		{"BlockPublicPolicy", b.BlockPublicPolicy},
		{"RestrictPublicBuckets", b.RestrictPublicBuckets},
	} {
		if !aws.ToBool(f.on) {
			off = append(off, f.name)
		}
	}
	if len(off) > 0 {
		return notPrivate(bucket, strings.Join(off, ", ")+" off")
	}
	return nil
}

func notPrivate(bucket, why string) error {
	return fmt.Errorf("bucket %s could be made public (%s), so sesh won't use it. To block public access:\n"+
		"  aws s3api put-public-access-block --bucket %s --public-access-block-configuration "+
		"BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true",
		bucket, why, bucket)
}
