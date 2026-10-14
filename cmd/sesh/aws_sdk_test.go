package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// sdkAdminAt returns an sdkAdmin whose requests go to handler.
func sdkAdminAt(t *testing.T, region string, handler http.HandlerFunc) *sdkAdmin {
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &sdkAdmin{pathStyle: true, cfg: aws.Config{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider("id", "secret", ""),
		BaseEndpoint: aws.String(srv.URL),
	}}
}

// The region of a bucket comes from S3's answer to HEAD: a 301 names it,
// and is asked again there; 404 means there is no bucket; 403 that it is
// someone else's.
func TestSDKBucketRegion(t *testing.T) {
	ctx := context.Background()
	// Each request's region is in its signature's credential scope.
	signed := func(r *http.Request) string {
		_, after, _ := strings.Cut(r.Header.Get("Authorization"), "Credential=")
		parts := strings.Split(after, "/")
		if len(parts) < 3 {
			return ""
		}
		return parts[2]
	}
	var asked []string
	a := sdkAdminAt(t, "", func(w http.ResponseWriter, r *http.Request) {
		region := signed(r)
		asked = append(asked, r.URL.Path+"@"+region)
		if r.Header.Get("X-Amz-Expected-Bucket-Owner") != "111122223333" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/elsewhere":
			w.Header().Set("X-Amz-Bucket-Region", "ap-south-1")
			if region != "ap-south-1" {
				w.WriteHeader(http.StatusMovedPermanently)
			}
		case "/here":
			w.Header().Set("X-Amz-Bucket-Region", "us-east-1")
		case "/theirs":
			w.Header().Set("X-Amz-Bucket-Region", "us-east-1")
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	for _, c := range []struct{ bucket, want, err string }{
		{"elsewhere", "ap-south-1", ""},
		{"here", "us-east-1", ""},
		{"none", "", ""},
		{"theirs", "", "not in your AWS account 111122223333"},
	} {
		got, err := a.BucketRegion(ctx, c.bucket, "111122223333")
		if got != c.want || (err == nil) != (c.err == "") || err != nil && !strings.Contains(err.Error(), c.err) {
			t.Errorf("BucketRegion(%s) = %q, %v; want %q, %q", c.bucket, got, err, c.want, c.err)
		}
	}
	if strings.Join(asked[:2], " ") != "/elsewhere@us-east-1 /elsewhere@ap-south-1" {
		t.Errorf("asked %v", asked)
	}
}

func TestSDKCreateBucket(t *testing.T) {
	var bodies []string
	a := sdkAdminAt(t, "us-east-1", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, r.Method+" "+r.URL.Path+" "+string(b))
	})
	ctx := context.Background()
	if err := a.CreateBucket(ctx, "b1", "us-east-1"); err != nil {
		t.Fatal(err)
	}
	if err := a.CreateBucket(ctx, "b2", "eu-west-1"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bodies[0], "LocationConstraint") || !strings.HasPrefix(bodies[0], "PUT /b1") {
		t.Errorf("us-east-1: %s", bodies[0])
	}
	if !strings.Contains(bodies[1], "<LocationConstraint>eu-west-1</LocationConstraint>") {
		t.Errorf("eu-west-1: %s", bodies[1])
	}
}

// An IAM user that exists already is fine; the access key comes back.
func TestSDKIAM(t *testing.T) {
	var actions []string
	a := sdkAdminAt(t, "", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		act := r.Form.Get("Action")
		actions = append(actions, act+":"+r.Form.Get("UserName")+":"+r.Form.Get("Path"))
		w.Header().Set("Content-Type", "text/xml")
		switch act {
		case "CreateUser":
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `<ErrorResponse><Error><Type>Sender</Type><Code>EntityAlreadyExists</Code><Message>exists</Message></Error><RequestId>1</RequestId></ErrorResponse>`)
		case "PutUserPolicy":
			if doc, _ := url.QueryUnescape(r.Form.Get("PolicyDocument")); !strings.Contains(doc, "s3:GetObject") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			io.WriteString(w, `<PutUserPolicyResponse><ResponseMetadata><RequestId>2</RequestId></ResponseMetadata></PutUserPolicyResponse>`)
		case "CreateAccessKey":
			io.WriteString(w, `<CreateAccessKeyResponse><CreateAccessKeyResult><AccessKey><UserName>u</UserName><AccessKeyId>made-id</AccessKeyId><Status>Active</Status><SecretAccessKey>made-secret</SecretAccessKey></AccessKey></CreateAccessKeyResult></CreateAccessKeyResponse>`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	ctx := context.Background()
	if err := a.EnsureUser(ctx, "sesh-b-v"); err != nil {
		t.Fatalf("EnsureUser of an existing user: %v", err)
	}
	if err := a.PutUserPolicy(ctx, "sesh-b-v", "sesh", awsTarget{bucket: "b", keys: "v"}.policy()); err != nil {
		t.Fatal(err)
	}
	id, secret, err := a.CreateAccessKey(ctx, "sesh-b-v")
	if err != nil || id != "made-id" || secret != "made-secret" {
		t.Fatalf("CreateAccessKey = %q, %q, %v", id, secret, err)
	}
	if strings.Join(actions, " ") != "CreateUser:sesh-b-v:/sesh/ PutUserPolicy:sesh-b-v: CreateAccessKey:sesh-b-v:" {
		t.Errorf("actions %v", actions)
	}
}

func TestSDKBlockPublicAccess(t *testing.T) {
	var got string
	a := sdkAdminAt(t, "us-east-1", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = r.Method + " " + r.URL.String() + " " + r.Header.Get("X-Amz-Expected-Bucket-Owner") + " " + string(b)
	})
	if err := a.BlockPublicAccess(context.Background(), "b1", "us-east-1", "111122223333"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PUT /b1?publicAccessBlock", " 111122223333 ",
		"<BlockPublicAcls>true</BlockPublicAcls>", "<IgnorePublicAcls>true</IgnorePublicAcls>",
		"<BlockPublicPolicy>true</BlockPublicPolicy>", "<RestrictPublicBuckets>true</RestrictPublicBuckets>"} {
		if !strings.Contains(got, want) {
			t.Errorf("request lacks %q:\n%s", want, got)
		}
	}
}

func TestSDKAccountID(t *testing.T) {
	a := sdkAdminAt(t, "", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("Action") != "GetCallerIdentity" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		io.WriteString(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Arn>arn:aws:iam::111122223333:user/admin</Arn><UserId>X</UserId><Account>111122223333</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`)
	})
	if id, err := a.AccountID(context.Background()); err != nil || id != "111122223333" {
		t.Errorf("AccountID = %q, %v", id, err)
	}
}
