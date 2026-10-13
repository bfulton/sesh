package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/bfulton/sesh/internal/store"
)

// awsAdmin is what sesh init -aws asks of AWS, with the credentials of
// whoever runs it: those other AWS tools use, such as AWS_PROFILE or
// ~/.aws/credentials. Tests replace it.
type awsAdmin interface {
	// Region is the region the credentials' configuration names, if any.
	Region() string
	// AccountID is the AWS account of the credentials.
	AccountID(ctx context.Context) (string, error)
	// BucketRegion returns the region of a bucket the account owns, or ""
	// if there is no such bucket.
	BucketRegion(ctx context.Context, bucket, owner string) (string, error)
	CreateBucket(ctx context.Context, bucket, region string) error
	// BlockPublicAccess turns on all four Block Public Access settings.
	BlockPublicAccess(ctx context.Context, bucket, region, owner string) error
	// CheckPrivate is store.CheckPrivate.
	CheckPrivate(ctx context.Context, bucket, region, owner string) error
	// EnsureUser creates an IAM user, unless it exists.
	EnsureUser(ctx context.Context, name string) error
	PutUserPolicy(ctx context.Context, user, name, doc string) error
	CreateAccessKey(ctx context.Context, user string) (id, secret string, err error)
}

var newAWSAdmin = func(ctx context.Context) (awsAdmin, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading your AWS configuration: %w", err)
	}
	return &sdkAdmin{cfg: cfg}, nil
}

// awsRetry is how often, and for how long, setup tries the new credentials:
// IAM takes a few seconds to make them work everywhere.
var awsRetry = struct{ every, limit time.Duration }{2 * time.Second, 90 * time.Second}

type sdkAdmin struct {
	cfg       aws.Config
	pathStyle bool // for tests
}

func (a *sdkAdmin) Region() string { return a.cfg.Region }

func (a *sdkAdmin) s3(region string) *s3.Client {
	return s3.NewFromConfig(a.cfg, func(o *s3.Options) {
		o.Region = region
		o.UsePathStyle = a.pathStyle
	})
}

func (a *sdkAdmin) AccountID(ctx context.Context) (string, error) {
	out, err := sts.NewFromConfig(a.cfg, func(o *sts.Options) {
		if o.Region == "" {
			o.Region = "us-east-1"
		}
	}).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("finding your AWS account: %w", err)
	}
	return aws.ToString(out.Account), nil
}

func (a *sdkAdmin) BucketRegion(ctx context.Context, bucket, owner string) (string, error) {
	probe := a.cfg.Region
	if probe == "" {
		probe = "us-east-1"
	}
	region, status, err := a.headBucket(ctx, bucket, probe, owner)
	if status == http.StatusMovedPermanently && region != "" {
		// In another region: ask there, to learn whether these credentials
		// can reach it.
		region, status, err = a.headBucket(ctx, bucket, region, owner)
	}
	switch {
	case status == http.StatusNotFound:
		return "", nil
	case status == http.StatusForbidden:
		return "", fmt.Errorf("bucket %s exists, but not in your AWS account %s, or your credentials can't reach it", bucket, owner)
	case err != nil:
		return "", fmt.Errorf("looking up bucket %s: %w", bucket, err)
	}
	return region, nil
}

// headBucket returns the bucket's region and the response status.
func (a *sdkAdmin) headBucket(ctx context.Context, bucket, region, owner string) (string, int, error) {
	out, err := a.s3(region).HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket:              aws.String(bucket),
		ExpectedBucketOwner: aws.String(owner),
	})
	if err == nil {
		if out.BucketRegion != nil {
			region = *out.BucketRegion
		}
		return region, http.StatusOK, nil
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.Response != nil {
		return re.Response.Header.Get("X-Amz-Bucket-Region"), re.HTTPStatusCode(), err
	}
	return "", 0, err
}

func (a *sdkAdmin) CreateBucket(ctx context.Context, bucket, region string) error {
	in := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	if region != "us-east-1" {
		in.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(region),
		}
	}
	_, err := a.s3(region).CreateBucket(ctx, in)
	return err
}

func (a *sdkAdmin) BlockPublicAccess(ctx context.Context, bucket, region, owner string) error {
	_, err := a.s3(region).PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket:              aws.String(bucket),
		ExpectedBucketOwner: aws.String(owner),
		PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
			BlockPublicAcls:       aws.Bool(true),
			IgnorePublicAcls:      aws.Bool(true),
			BlockPublicPolicy:     aws.Bool(true),
			RestrictPublicBuckets: aws.Bool(true),
		},
	})
	return err
}

func (a *sdkAdmin) CheckPrivate(ctx context.Context, bucket, region, owner string) error {
	return store.CheckPrivate(ctx, a.s3(region), bucket, owner)
}

func (a *sdkAdmin) iam() *iam.Client {
	return iam.NewFromConfig(a.cfg, func(o *iam.Options) {
		if o.Region == "" {
			o.Region = "us-east-1" // IAM is global
		}
	})
}

func (a *sdkAdmin) EnsureUser(ctx context.Context, name string) error {
	_, err := a.iam().CreateUser(ctx, &iam.CreateUserInput{
		UserName: aws.String(name),
		Path:     aws.String("/sesh/"),
	})
	var exists *iamtypes.EntityAlreadyExistsException
	if errors.As(err, &exists) {
		return nil
	}
	return err
}

func (a *sdkAdmin) PutUserPolicy(ctx context.Context, user, name, doc string) error {
	_, err := a.iam().PutUserPolicy(ctx, &iam.PutUserPolicyInput{
		UserName:       aws.String(user),
		PolicyName:     aws.String(name),
		PolicyDocument: aws.String(doc),
	})
	return err
}

func (a *sdkAdmin) CreateAccessKey(ctx context.Context, user string) (string, string, error) {
	out, err := a.iam().CreateAccessKey(ctx, &iam.CreateAccessKeyInput{UserName: aws.String(user)})
	if err != nil {
		return "", "", err
	}
	return aws.ToString(out.AccessKey.AccessKeyId), aws.ToString(out.AccessKey.SecretAccessKey), nil
}

// validBucket matches an S3 bucket name.
var validBucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// validPrefix matches a key prefix: path segments of characters safe in a
// key, an IAM policy and a URL.
var validPrefix = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)

// awsTarget is where in S3 a vault lives.
type awsTarget struct {
	bucket string
	keys   string // the key prefix: the given prefix, then the vault name
}

func parseAWSTarget(arg, vault string) (awsTarget, error) {
	bucket, prefix, _ := strings.Cut(strings.TrimPrefix(arg, "s3://"), "/")
	prefix = strings.Trim(prefix, "/")
	if !validBucket.MatchString(bucket) {
		return awsTarget{}, fmt.Errorf("bucket name %q: use 3 to 63 lowercase letters, digits, dots and hyphens", bucket)
	}
	if prefix != "" && (!validPrefix.MatchString(prefix) || strings.Contains("/"+prefix+"/", "/./") || strings.Contains("/"+prefix+"/", "/../")) {
		return awsTarget{}, fmt.Errorf("prefix %q: use letters, digits, ., - and _, in parts separated by /", prefix)
	}
	keys := vault
	if prefix != "" {
		keys = prefix + "/" + vault
	}
	return awsTarget{bucket: bucket, keys: keys}, nil
}

func (t awsTarget) storeURL() string { return "s3://" + t.bucket + "/" + t.keys }

// userName is the IAM user of the vault: one per bucket, prefix and vault.
// IAM user names have at most 64 characters, of which / isn't one.
func (t awsTarget) userName() string {
	n := "sesh-" + t.bucket + "-" + strings.ReplaceAll(t.keys, "/", "-")
	if len(n) <= 64 {
		return n
	}
	sum := sha256.Sum256([]byte(t.bucket + "/" + t.keys))
	vault := t.keys[strings.LastIndexByte(t.keys, '/')+1:]
	return "sesh-" + vault + "-" + hex.EncodeToString(sum[:8])
}

// policy lets the vault's user read, write and list its own keys, and check
// that the bucket isn't public; nothing else. sesh never deletes.
func (t awsTarget) policy() string {
	type stmt struct {
		Effect    string
		Action    []string
		Resource  string
		Condition map[string]map[string]string `json:",omitempty"`
	}
	doc := struct {
		Version   string
		Statement []stmt
	}{"2012-10-17", []stmt{
		{Effect: "Allow", Action: []string{"s3:GetObject", "s3:PutObject"},
			Resource: "arn:aws:s3:::" + t.bucket + "/" + t.keys + "/*"},
		{Effect: "Allow", Action: []string{"s3:ListBucket"},
			Resource:  "arn:aws:s3:::" + t.bucket,
			Condition: map[string]map[string]string{"StringLike": {"s3:prefix": t.keys + "/*"}}},
		{Effect: "Allow", Action: []string{"s3:GetBucketPublicAccessBlock"},
			Resource: "arn:aws:s3:::" + t.bucket},
	}}
	b, _ := json.Marshal(doc)
	return string(b)
}

// setupAWS makes the bucket if there is none, and checks that it blocks
// public access; then the vault's IAM user and its policy, and, unless cfg
// already holds credentials for this store, an access key. It sets cfg's
// store and aws_ settings.
func setupAWS(ctx context.Context, cfg map[string]string, t awsTarget, region string) error {
	admin, err := newAWSAdmin(ctx)
	if err != nil {
		return err
	}
	account, err := admin.AccountID(ctx)
	if err != nil {
		return err
	}
	have, err := admin.BucketRegion(ctx, t.bucket, account)
	if err != nil {
		return err
	}
	switch {
	case have == "":
		if region == "" {
			region = admin.Region()
		}
		if region == "" {
			region = "us-east-1"
		}
		if err := admin.CreateBucket(ctx, t.bucket, region); err != nil {
			return fmt.Errorf("creating bucket %s: %w", t.bucket, err)
		}
		// New buckets block public access by default; this doesn't rely on it.
		if err := admin.BlockPublicAccess(ctx, t.bucket, region, account); err != nil {
			return fmt.Errorf("blocking public access to bucket %s: %w", t.bucket, err)
		}
		fmt.Printf("Created bucket %s in %s.\n", t.bucket, region)
	case region != "" && region != have:
		return fmt.Errorf("bucket %s is in %s, not %s", t.bucket, have, region)
	default:
		region = have
	}
	if err := admin.CheckPrivate(ctx, t.bucket, region, account); err != nil {
		return err
	}

	user := t.userName()
	if err := admin.EnsureUser(ctx, user); err != nil {
		return fmt.Errorf("creating IAM user %s: %w", user, err)
	}
	if err := admin.PutUserPolicy(ctx, user, "sesh", t.policy()); err != nil {
		return fmt.Errorf("setting the policy of IAM user %s: %w", user, err)
	}
	reuse := cfg["store"] == t.storeURL() && cfg["aws_access_key_id"] != "" && cfg["aws_secret_access_key"] != ""
	if !reuse {
		id, secret, err := admin.CreateAccessKey(ctx, user)
		if err != nil {
			return fmt.Errorf("creating an access key for IAM user %s: %w", user, err)
		}
		cfg["aws_access_key_id"], cfg["aws_secret_access_key"] = id, secret
	}
	cfg["store"] = t.storeURL()
	cfg["aws_region"] = region
	cfg["aws_account_id"] = account
	for _, k := range []string{"aws_session_token", "aws_endpoint_url_s3", "s3_path_style"} {
		delete(cfg, k)
	}
	fmt.Printf("IAM user %s can read and write %s.\n", user, t.storeURL())
	return nil
}

// awsCheckEndpoint is where checkS3 finds S3 in tests.
var awsCheckEndpoint string

// checkS3 waits until the vault's credentials can list its keys, and check
// that the bucket blocks public access.
func checkS3(ctx context.Context, cfg map[string]string) error {
	c := store.S3Config{
		AccessKeyID:     cfg["aws_access_key_id"],
		SecretAccessKey: cfg["aws_secret_access_key"],
		Region:          cfg["aws_region"],
		AccountID:       cfg["aws_account_id"],
	}
	if awsCheckEndpoint != "" {
		c.Endpoint, c.PathStyle = awsCheckEndpoint, true
	}
	st, err := store.Open(cfg["store"], c)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(awsRetry.limit)
	for {
		_, err := st.List(ctx, "v1/")
		if err == nil {
			err = st.(*store.S3).CheckPrivate(ctx)
		}
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the vault's credentials can't list %s: %w", cfg["store"], err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(awsRetry.every):
		}
	}
}
