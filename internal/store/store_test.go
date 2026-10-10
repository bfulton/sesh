package store

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

const id = "0123456789abcdef0123456789abcdef"

func exercise(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Get(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get before Put: err = %v, want ErrNotFound", err)
	}
	for _, data := range [][]byte{[]byte("first"), bytes.Repeat([]byte{0, 255}, 1<<16)} {
		if err := s.Put(ctx, id, data); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("Get returned %d bytes, want %d", len(got), len(data))
		}
	}
	if err := s.Put(ctx, "../../etc/passwd", nil); err == nil {
		t.Error("Put accepted an invalid id")
	}
}

func TestDir(t *testing.T) {
	exercise(t, &Dir{Root: t.TempDir()})
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
	exercise(t, &S3{Client: client, Bucket: "sesh-test", Prefix: "team/"})

	// The object lands under the prefix at sesh/v1/<id>.
	if _, err := backend.HeadObject("sesh-test", "team/sesh/v1/"+id); err != nil {
		t.Errorf("object not at expected key: %v", err)
	}
}

func TestOpen(t *testing.T) {
	if s, err := Open("file:///tmp/x"); err != nil || s.(*Dir).Root != "/tmp/x" {
		t.Errorf("file URL: %v %v", s, err)
	}
	for _, bad := range []string{"file://", "s3://", "http://x", "nope"} {
		if _, err := Open(bad); err == nil {
			t.Errorf("Open(%q) succeeded", bad)
		}
	}
}
