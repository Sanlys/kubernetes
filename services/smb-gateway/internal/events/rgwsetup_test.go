package events

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/config"
)

// TestEnsureRGW checks the request shapes against an S3+SNS mock (moto), and
// that existing notification configs survive and setup is idempotent. Set
// MOTO_URL (e.g. http://127.0.0.1:5000) to run it.
func TestEnsureRGW(t *testing.T) {
	url := os.Getenv("MOTO_URL")
	if url == "" {
		t.Skip("MOTO_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	creds := config.S3{Endpoint: url, Region: "us-east-1", AccessKeyID: "test", SecretAccessKey: "test"}
	c, err := config.Parse([]byte(`
server: {name: testgw}
s3: {endpoint: "` + url + `", access_key_id: test, secret_access_key: test}
notifications: {endpoint_url: "http://gw:8090", token: 0123456789abcdef}
mounts: [{path: /a, type: s3, bucket: bkt}]
users: [{name: u, password: p}]
shares: [{name: s, path: /, read: [u]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	s3c := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", "")},
		func(o *s3.Options) { o.BaseEndpoint = aws.String(url); o.UsePathStyle = true })
	if _, err := s3c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("bkt")}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := BucketSetup{Bucket: "bkt", S3: creds}
	if err := EnsureRGW(ctx, c, b, log); err != nil {
		t.Fatal(err)
	}
	// Someone else's notification, added later, must survive a re-run.
	cur, _ := s3c.GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{Bucket: aws.String("bkt")})
	if len(cur.TopicConfigurations) != 1 {
		t.Fatalf("want 1 topic config, got %d", len(cur.TopicConfigurations))
	}
	other := s3types.TopicConfiguration{Id: aws.String("other"), TopicArn: cur.TopicConfigurations[0].TopicArn, Events: []s3types.Event{"s3:ObjectRemoved:*"}}
	_, err = s3c.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{Bucket: aws.String("bkt"),
		NotificationConfiguration: &s3types.NotificationConfiguration{TopicConfigurations: append(cur.TopicConfigurations, other)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureRGW(ctx, c, b, log); err != nil {
		t.Fatal(err)
	}
	cur, _ = s3c.GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{Bucket: aws.String("bkt")})
	ids := map[string]bool{}
	for _, tc := range cur.TopicConfigurations {
		ids[aws.ToString(tc.Id)] = true
	}
	if len(cur.TopicConfigurations) != 2 || !ids["other"] || !ids["smb-gateway-testgw"] {
		t.Fatalf("unexpected configs after re-run: %v", ids)
	}
}
