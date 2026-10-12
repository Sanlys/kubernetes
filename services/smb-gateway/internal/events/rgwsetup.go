package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/smithy-go"

	"github.com/Sanlys/kubernetes/services/smb-gateway/internal/config"
)

// BucketSetup is the RGW-side configuration one bucket needs to push its
// changes to the gateway.
type BucketSetup struct {
	Bucket string
	S3     config.S3
}

// EnsureRGW creates (or updates) the gateway's SNS topic on RGW and attaches
// a bucket notification for it to each bucket. Other notification
// configurations on the bucket are preserved.
//
// The topic is persistent: RGW queues events and delivers them
// asynchronously. A non-persistent topic would make every S3 write in the
// cluster wait for this gateway to acknowledge the event.
func EnsureRGW(ctx context.Context, c *config.Config, b BucketSetup, log *slog.Logger) error {
	n := c.Notifications
	cfg := aws.Config{
		Region:      b.S3.Region,
		Credentials: credentials.NewStaticCredentialsProvider(b.S3.AccessKeyID, b.S3.SecretAccessKey, ""),
		// Newer SDKs add CRC checksum headers to every request by default,
		// which older RGW releases reject.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	topicName := c.Server.Name
	attrs := map[string]string{
		"push-endpoint":        strings.TrimRight(n.EndpointURL, "/") + "/rgw/" + n.Token,
		"persistent":           "true",
		"time_to_live":         fmt.Sprint(int(n.TopicTTL.D().Seconds())),
		"max_retries":          "100",
		"retry_sleep_duration": "10",
	}
	snsc := sns.NewFromConfig(cfg, func(o *sns.Options) { o.BaseEndpoint = aws.String(b.S3.Endpoint) })
	out, err := snsc.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String(topicName), Attributes: attrs})
	if err != nil {
		// Older RGW releases reject the retry attributes; try without.
		delete(attrs, "time_to_live")
		delete(attrs, "max_retries")
		delete(attrs, "retry_sleep_duration")
		out, err = snsc.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String(topicName), Attributes: attrs})
		if err != nil {
			return fmt.Errorf("creating topic %s: %w", topicName, err)
		}
	}
	arn := aws.ToString(out.TopicArn)

	s3c := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(b.S3.Endpoint)
		o.UsePathStyle = true
	})
	id := "smb-gateway-" + c.Server.Name
	cur, err := s3c.GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{Bucket: aws.String(b.Bucket)})
	if err != nil {
		var ae smithy.APIError
		if !errors.As(err, &ae) || ae.ErrorCode() != "NoSuchKey" {
			return fmt.Errorf("reading notifications of %s: %w", b.Bucket, err)
		}
		cur = &s3.GetBucketNotificationConfigurationOutput{}
	}
	var topics []s3types.TopicConfiguration
	for _, t := range cur.TopicConfigurations {
		if aws.ToString(t.Id) == id {
			if aws.ToString(t.TopicArn) == arn && len(t.Events) == 2 {
				log.Info("bucket notification already configured", "bucket", b.Bucket, "topic", arn)
				return nil
			}
			continue
		}
		topics = append(topics, t)
	}
	topics = append(topics, s3types.TopicConfiguration{
		Id:       aws.String(id),
		TopicArn: aws.String(arn),
		Events:   []s3types.Event{"s3:ObjectCreated:*", "s3:ObjectRemoved:*"},
	})
	_, err = s3c.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{
		Bucket: aws.String(b.Bucket),
		NotificationConfiguration: &s3types.NotificationConfiguration{
			TopicConfigurations:          topics,
			QueueConfigurations:          cur.QueueConfigurations,
			LambdaFunctionConfigurations: cur.LambdaFunctionConfigurations,
		},
	})
	if err != nil {
		return fmt.Errorf("configuring notification on %s: %w", b.Bucket, err)
	}
	log.Info("bucket notification configured", "bucket", b.Bucket, "topic", arn)
	return nil
}

// EnsureRGWLoop retries EnsureRGW until it succeeds or ctx ends, reporting
// state through ok.
func EnsureRGWLoop(ctx context.Context, c *config.Config, b BucketSetup, log *slog.Logger, ok func(bool, error)) {
	backoff := 10 * time.Second
	for {
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		err := EnsureRGW(cctx, c, b, log)
		cancel()
		ok(err == nil, err)
		if err == nil {
			return
		}
		log.Error("RGW notification setup failed, will retry", "bucket", b.Bucket, "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Minute {
			backoff *= 2
		}
	}
}
