package store

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

const name = "v1/s/0123456789abcdef0123456789abcdef"

// Exercise checks the behavior every Store must have.
func Exercise(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Get(ctx, name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get before Put: err = %v, want ErrNotFound", err)
	}
	for _, data := range [][]byte{[]byte("first"), bytes.Repeat([]byte{0, 255}, 1<<16)} {
		if err := s.Put(ctx, name, data); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("Get returned %d bytes, want %d", len(got), len(data))
		}
	}
	for _, n := range []string{"v1/c/aa/1", "v1/c/aa/2", "v1/c/bb/1"} {
		if err := s.Put(ctx, n, []byte(n)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List(ctx, "v1/c/aa/")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"v1/c/aa/1", "v1/c/aa/2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}
	for _, bad := range []string{"../../etc/passwd", "v1/S/x", "/v1/x", "v1//x", ""} {
		if err := s.Put(ctx, bad, nil); err == nil {
			t.Errorf("Put accepted invalid name %q", bad)
		}
	}
}

func TestDir(t *testing.T) {
	Exercise(t, &Dir{Root: t.TempDir()})
}

func TestS3(t *testing.T) {
	backend := s3mem.New()
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	defer srv.Close()
	if err := backend.CreateBucket("sesh-test"); err != nil {
		t.Fatal(err)
	}
	client := s3.New(s3.Options{
		Region:       "auto",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("id", "secret", ""),
	})
	Exercise(t, &S3{Client: client, Bucket: "sesh-test", Prefix: "team"})
	if _, err := backend.HeadObject("sesh-test", "team/"+name); err != nil {
		t.Errorf("object not at expected key: %v", err)
	}
}

func TestOpen(t *testing.T) {
	if s, err := Open("file:///tmp/x", S3Config{}); err != nil || s.(*Dir).Root != "/tmp/x" {
		t.Errorf("file URL: %v %v", s, err)
	}
	if s, err := Open("https://sesh.example/", S3Config{}); err != nil || s.(*Remote).Base.Host != "sesh.example" {
		t.Errorf("https URL: %v %v", s, err)
	}
	for _, bad := range []string{"file://", "s3://", "ftp://x", "nope"} {
		if _, err := Open(bad, S3Config{}); err == nil {
			t.Errorf("Open(%q) succeeded", bad)
		}
	}
}

// NewS3 uses the config it is given, and none of the AWS_ variables and
// files other tools share.
func TestNewS3UsesItsConfig(t *testing.T) {
	backend := s3mem.New()
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	defer srv.Close()
	if err := backend.CreateBucket("sesh-test"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "aws-id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_ENDPOINT_URL_S3", "http://aws.invalid")
	if _, err := NewS3("sesh-test", "team", S3Config{}); err == nil {
		t.Fatal("NewS3 used the AWS_ variables")
	}

	c := S3Config{AccessKeyID: "sesh-id", SecretAccessKey: "sesh-secret", Endpoint: srv.URL, PathStyle: true}
	s, err := NewS3("sesh-test", "/team/", c)
	if err != nil {
		t.Fatal(err)
	}
	o := s.Client.Options()
	creds, err := o.Credentials.Retrieve(context.Background())
	if err != nil || creds.AccessKeyID != "sesh-id" || creds.SecretAccessKey != "sesh-secret" {
		t.Errorf("credentials %q, %v; want sesh-id", creds.AccessKeyID, err)
	}
	if o.Region != "auto" || aws.ToString(o.BaseEndpoint) != srv.URL || !o.UsePathStyle {
		t.Errorf("region %q, endpoint %q, path style %v", o.Region, aws.ToString(o.BaseEndpoint), o.UsePathStyle)
	}
	Exercise(t, s)
	if _, err := backend.HeadObject("sesh-test", "team/"+name); err != nil {
		t.Errorf("object not at expected key: %v", err)
	}

	c.Region = "us-west-2"
	if s, _ := NewS3("sesh-test", "", c); s.Client.Options().Region != "us-west-2" {
		t.Errorf("Region ignored")
	}
	c.SecretAccessKey = ""
	if _, err := NewS3("sesh-test", "", c); err == nil {
		t.Error("NewS3 accepted an access key ID without its secret")
	}
}
