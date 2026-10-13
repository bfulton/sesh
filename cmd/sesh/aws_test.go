package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/bfulton/sesh/internal/store"
)

// fakeAWS records what sesh init -aws asks of AWS, and keeps buckets in a
// gofakes3 backend.
type fakeAWS struct {
	region   string
	account  string
	backend  *s3mem.Backend
	buckets  map[string]string // bucket → region
	owners   map[string]string // bucket → account
	private  map[string]bool   // bucket → all four Block Public Access settings on
	public   map[string]bool   // bucket → no Block Public Access settings at all
	users    map[string]bool
	policies map[string]string // user → policy document
	keys     map[string]int    // user → access keys made
}

func (f *fakeAWS) Region() string { return f.region }

func (f *fakeAWS) AccountID(context.Context) (string, error) { return f.account, nil }

func (f *fakeAWS) BucketRegion(_ context.Context, b, owner string) (string, error) {
	if f.buckets[b] != "" && f.owners[b] != owner {
		return "", errors.New("another account's bucket")
	}
	return f.buckets[b], nil
}

func (f *fakeAWS) CreateBucket(_ context.Context, b, region string) error {
	f.buckets[b], f.owners[b] = region, f.account
	return f.backend.CreateBucket(b)
}

func (f *fakeAWS) BlockPublicAccess(_ context.Context, b, _, owner string) error {
	if f.owners[b] != owner {
		return errors.New("access denied")
	}
	f.private[b], f.public[b] = true, false
	return nil
}

// CheckPrivate asks the fake S3 server, as the real one asks S3.
func (f *fakeAWS) CheckPrivate(ctx context.Context, b, _, owner string) error {
	c := s3.New(s3.Options{Region: "auto", BaseEndpoint: aws.String(awsCheckEndpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider("admin", "admin", "")})
	return store.CheckPrivate(ctx, c, b, owner)
}

// addBucket adds a bucket of account, private or not.
func (f *fakeAWS) addBucket(b, region, account string, private bool) {
	f.buckets[b], f.owners[b], f.private[b] = region, account, private
	f.backend.CreateBucket(b)
}

func (f *fakeAWS) EnsureUser(_ context.Context, name string) error {
	f.users[name] = true
	return nil
}

func (f *fakeAWS) PutUserPolicy(_ context.Context, user, name, doc string) error {
	if !f.users[user] || name != "sesh" {
		return errors.New("no such user")
	}
	f.policies[user] = doc
	return nil
}

func (f *fakeAWS) CreateAccessKey(_ context.Context, user string) (string, string, error) {
	f.keys[user]++
	// Made at run time, so no credential-shaped string sits in the source.
	return fmt.Sprintf("FAKEKEYID%s%d", strings.ToUpper(user[len(user)-3:]), f.keys[user]),
		fmt.Sprintf("fake-secret-%d-%d", time.Now().UnixNano(), f.keys[user]), nil
}

// setupFakeAWS replaces AWS with a fake whose S3 refuses the first
// refusals requests, as S3 does while new IAM credentials propagate. Its S3
// also checks the expected bucket owner, and answers for each bucket's
// Block Public Access settings, which gofakes3 doesn't.
func setupFakeAWS(t *testing.T, refusals int32) *fakeAWS {
	t.Helper()
	f := &fakeAWS{region: "eu-west-1", account: "111122223333", backend: s3mem.New(),
		buckets: map[string]string{}, owners: map[string]string{}, private: map[string]bool{}, public: map[string]bool{},
		users: map[string]bool{}, policies: map[string]string{}, keys: map[string]int{}}
	fake := gofakes3.New(f.backend).Server()
	var n atomic.Int32
	s3err := func(w http.ResponseWriter, status int, code string) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, code)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the new access key is refused; the admin's works at once.
		admin := strings.Contains(r.Header.Get("Authorization"), "Credential=admin/")
		if !admin && n.Add(1) <= refusals {
			s3err(w, http.StatusForbidden, "InvalidAccessKeyId")
			return
		}
		bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if o := r.Header.Get("X-Amz-Expected-Bucket-Owner"); o != "" && o != f.owners[bucket] {
			s3err(w, http.StatusForbidden, "AccessDenied")
			return
		}
		if _, ok := r.URL.Query()["publicAccessBlock"]; ok && r.Method == http.MethodGet {
			if f.public[bucket] {
				s3err(w, http.StatusNotFound, "NoSuchPublicAccessBlockConfiguration")
				return
			}
			p := f.private[bucket]
			fmt.Fprintf(w, `<PublicAccessBlockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+
				`<BlockPublicAcls>true</BlockPublicAcls><IgnorePublicAcls>%v</IgnorePublicAcls>`+
				`<BlockPublicPolicy>true</BlockPublicPolicy><RestrictPublicBuckets>%v</RestrictPublicBuckets>`+
				`</PublicAccessBlockConfiguration>`, p, p)
			return
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	oldAdmin, oldEndpoint, oldRetry := newAWSAdmin, awsCheckEndpoint, awsRetry
	t.Cleanup(func() { newAWSAdmin, awsCheckEndpoint, awsRetry = oldAdmin, oldEndpoint, oldRetry })
	newAWSAdmin = func(context.Context) (awsAdmin, error) { return f, nil }
	awsCheckEndpoint = srv.URL
	checkAllBuckets = true
	t.Cleanup(func() { checkAllBuckets = false })
	awsRetry.every, awsRetry.limit = time.Millisecond, 5*time.Second
	for _, k := range configKeys {
		t.Setenv(k.env, "")
	}
	t.Setenv("SESH_KEY", "")
	t.Setenv("SESH_VAULT", "")
	t.Setenv("SESH_HOME", t.TempDir())
	return f
}

func TestInitAWS(t *testing.T) {
	f := setupFakeAWS(t, 2)
	claudeEnv(t, false, false)
	out, _, err := runCmd(t, "init", "-aws", "my-bucket/team/x", "vault01")
	if err != nil {
		t.Fatal(err)
	}
	if f.buckets["my-bucket"] != "eu-west-1" {
		t.Errorf("bucket made in %q, want the configured region eu-west-1", f.buckets["my-bucket"])
	}
	user := "sesh-my-bucket-team-x-vault01"
	if !f.users[user] || f.keys[user] != 1 {
		t.Fatalf("users %v, keys %v", f.users, f.keys)
	}
	var doc struct {
		Statement []struct {
			Action    []string
			Resource  string
			Condition map[string]map[string]string
		}
	}
	if err := json.Unmarshal([]byte(f.policies[user]), &doc); err != nil || len(doc.Statement) != 3 {
		t.Fatalf("policy %s: %v", f.policies[user], err)
	}
	if s := doc.Statement[0]; strings.Join(s.Action, ",") != "s3:GetObject,s3:PutObject" || s.Resource != "arn:aws:s3:::my-bucket/team/x/vault01/*" {
		t.Errorf("object statement %+v", s)
	}
	if s := doc.Statement[1]; strings.Join(s.Action, ",") != "s3:ListBucket" || s.Resource != "arn:aws:s3:::my-bucket" ||
		s.Condition["StringLike"]["s3:prefix"] != "team/x/vault01/*" {
		t.Errorf("list statement %+v", s)
	}
	if s := doc.Statement[2]; strings.Join(s.Action, ",") != "s3:GetBucketPublicAccessBlock" || s.Resource != "arn:aws:s3:::my-bucket" || s.Condition != nil {
		t.Errorf("check statement %+v", s)
	}
	if !f.private["my-bucket"] {
		t.Error("a new bucket's public access isn't blocked")
	}

	dir := filepath.Join(os.Getenv("SESH_HOME"), "vault01")
	if _, err := os.Stat(filepath.Join(dir, "key")); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(dir, "config")
	if fi, err := os.Stat(cfgFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config: %v %v", fi, err)
	}
	t.Setenv("SESH_VAULT", "vault01")
	cfg := readConfig()
	if cfg["store"] != "s3://my-bucket/team/x/vault01" || cfg["aws_region"] != "eu-west-1" || cfg["aws_account_id"] != f.account || cfg["aws_access_key_id"] == "" || cfg["aws_secret_access_key"] == "" {
		t.Errorf("config %v", redactedCfg(cfg))
	}
	if !strings.Contains(out, cfg["store"]) {
		t.Errorf("output:\n%s", out)
	}

	// The vault's store keeps its objects under the vault's prefix.
	awsCheckEndpointForStore(t)
	st, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	const obj = "v1/s/0123456789abcdef0123456789abcdef"
	if err := st.Put(context.Background(), obj, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.backend.HeadObject("my-bucket", "team/x/vault01/"+obj); err != nil {
		t.Errorf("object not under the vault's prefix: %v", err)
	}

	// A bucket made public since is refused, as is one that changed hands.
	f.private["my-bucket"] = false
	if _, err := openStore(); err == nil || !strings.Contains(err.Error(), "IgnorePublicAcls, RestrictPublicBuckets off") {
		t.Errorf("openStore of a public bucket: %v", err)
	}
	f.public["my-bucket"] = true
	if _, err := openStore(); err == nil || !strings.Contains(err.Error(), "no Block Public Access settings") {
		t.Errorf("openStore of a bucket without Block Public Access: %v", err)
	}
	f.private["my-bucket"], f.public["my-bucket"] = true, false
	f.owners["my-bucket"] = "444455556666"
	if _, err := openStore(); err == nil || !strings.Contains(err.Error(), "must belong to AWS account "+f.account) {
		t.Errorf("openStore of another account's bucket: %v", err)
	}
	if err := st.Put(context.Background(), obj, []byte("y")); err == nil {
		t.Error("Put to another account's bucket succeeded")
	}
	f.owners["my-bucket"] = f.account

	// Again, with the same store: the policy is set again, and the
	// access key kept.
	if _, _, err := runCmd(t, "init", "vault01", "-aws", "s3://my-bucket/team/x/"); err != nil {
		t.Fatal(err)
	}
	if f.keys[user] != 1 {
		t.Errorf("a second init made another access key: %d", f.keys[user])
	}

	// The default vault, in the same bucket, gets its own user.
	t.Setenv("SESH_VAULT", "")
	claudeEnv(t, true, false)
	out, _, err = runCmd(t, "init", "-aws", "my-bucket")
	if err != nil {
		t.Fatal(err)
	}
	if !f.users["sesh-my-bucket-default"] || readConfig()["store"] != "s3://my-bucket/default" {
		t.Errorf("default vault: users %v, store %q", f.users, readConfig()["store"])
	}
	for _, v := range readConfig() {
		if len(v) > 8 && strings.Contains(out, v) && !strings.HasPrefix(v, "s3://") && v != "eu-west-1" {
			t.Errorf("init inside Claude Code printed a setting:\n%s", out)
		}
	}
	key, _ := os.ReadFile(filepath.Join(os.Getenv("SESH_HOME"), "default", "key"))
	if strings.Contains(out, strings.TrimSpace(string(key))) {
		t.Error("init inside Claude Code printed the key")
	}
}

// awsCheckEndpointForStore points the vault's store at the fake S3 too.
func awsCheckEndpointForStore(t *testing.T) {
	t.Setenv("SESH_AWS_ENDPOINT_URL_S3", awsCheckEndpoint)
	t.Setenv("SESH_S3_PATH_STYLE", "1")
}

func redactedCfg(cfg map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range cfg {
		if strings.Contains(k, "secret") {
			v = "…"
		}
		out[k] = v
	}
	return out
}

func TestInitAWSRefusals(t *testing.T) {
	f := setupFakeAWS(t, 0)
	claudeEnv(t, false, false)
	f.addBucket("theirs", "us-west-2", f.account, true)
	f.addBucket("public", "us-west-2", f.account, false)
	f.addBucket("other-account", "us-west-2", "444455556666", true)
	for _, args := range [][]string{
		{"init", "-aws", "theirs", "-region", "eu-central-1"}, // in another region
		{"init", "-aws", "public"},
		{"init", "-aws", "other-account"},
		{"init", "-aws", "Bad_Bucket"},
		{"init", "-aws", "ok-bucket/../x"},
		{"init", "-aws", "ok-bucket/a/./x"},
		{"init", "-aws", "ok-bucket", "-store", "file:///tmp/x"},
		{"init", "-region", "us-west-2"},
		{"init", "Vault"},
		{"init", "a", "b"[:1] + "/"},
	} {
		if _, _, err := runCmd(t, args...); err == nil {
			t.Errorf("sesh %s succeeded", strings.Join(args, " "))
		}
	}
	if len(f.users) != 0 {
		t.Errorf("a refused init made users: %v", f.users)
	}

	// An existing bucket keeps its region.
	if _, _, err := runCmd(t, "init", "-aws", "theirs"); err != nil {
		t.Fatal(err)
	}
	if readConfig()["aws_region"] != "us-west-2" {
		t.Errorf("region %q, want the bucket's", readConfig()["aws_region"])
	}

	// S3 that never accepts the new credentials fails init, but keeps them.
	f2 := setupFakeAWS(t, 1<<30)
	awsRetry.limit = 20 * time.Millisecond
	_, _, err := runCmd(t, "init", "-aws", "slow-bucket")
	if err == nil || !strings.Contains(err.Error(), "can't list") {
		t.Fatalf("init with refused credentials: %v", err)
	}
	if readConfig()["aws_access_key_id"] == "" || f2.keys["sesh-slow-bucket-default"] != 1 {
		t.Error("credentials lost after a failed check")
	}
}

func TestAWSUserName(t *testing.T) {
	long := awsTarget{bucket: strings.Repeat("b", 63), keys: "some/prefix/vault01"}
	n := long.userName()
	if len(n) > 64 || !strings.HasPrefix(n, "sesh-vault01-") {
		t.Errorf("userName = %q (%d)", n, len(n))
	}
	other := awsTarget{bucket: strings.Repeat("b", 63), keys: "other/prefix/vault01"}
	if other.userName() == n {
		t.Error("two stores share an IAM user")
	}
}

func TestEnv(t *testing.T) {
	setupFakeAWS(t, 0)
	claudeEnv(t, false, false)
	if _, _, err := runCmd(t, "init"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCmd(t, "env"); err == nil || !strings.Contains(err.Error(), "on this device") {
		t.Errorf("env with a file store: %v", err)
	}
	if _, _, err := runCmd(t, "init", "-aws", "env-bucket"); err != nil {
		t.Fatal(err)
	}
	claudeEnv(t, true, false)
	if out, _, err := runCmd(t, "env"); err == nil || out != "" {
		t.Errorf("env inside Claude Code: %v, %q", err, out)
	}
	claudeEnv(t, false, false)
	out, _, err := runCmd(t, "env")
	if err != nil {
		t.Fatal(err)
	}
	cfg := readConfig()
	key, _ := os.ReadFile(filepath.Join(vaultDir(), "key"))
	want := "SESH_KEY=" + strings.TrimSpace(string(key)) + "\n" +
		"SESH_STORE=s3://env-bucket/default\n" +
		"SESH_AWS_ACCESS_KEY_ID=" + cfg["aws_access_key_id"] + "\n" +
		"SESH_AWS_SECRET_ACCESS_KEY=" + cfg["aws_secret_access_key"] + "\n" +
		"SESH_AWS_REGION=eu-west-1\n" +
		"SESH_AWS_ACCOUNT_ID=111122223333\n"
	if out != want {
		t.Errorf("env printed:\n%s\nwant:\n%s", out, want)
	}
}
