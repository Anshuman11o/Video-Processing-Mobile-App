// Package queue provides SQS operations for the DayReel worker pipeline.
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/anshumanagarwal/dayreel/internal/config"
)

const (
	// longPollSeconds is the SQS receive wait time. Long polling is what keeps
	// the consume loop from spinning: a receive with wait time 0 returns
	// immediately and the loop burns CPU against an empty queue.
	longPollSeconds = 20

	// maxMessages is how many messages a single receive pulls. One at a time
	// keeps the failure story simple; parallelism comes later.
	maxMessages = 1
)

// Message is a received SQS message, flattened to the fields workers need.
type Message struct {
	Body          string
	ReceiptHandle string
	// ReceiveCount is the SQS ApproximateReceiveCount, i.e. how many times this
	// message has been delivered. It is the source of truth for a stage's
	// attempt number, since the S3-triggered path carries no attempt in-band.
	ReceiveCount int
}

// Client wraps the AWS SQS client and caches resolved queue URLs.
type Client struct {
	sqs *sqs.Client

	mu   sync.RWMutex
	urls map[string]string
}

// New creates a Client, configured for LocalStack when the config says so.
func New(ctx context.Context, cfg *config.Config) (*Client, error) {
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.AWSRegion),
	}

	if cfg.UseLocalStack && cfg.AWSEndpoint != "" {
		opts = append(opts,
			awsconfig.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider("test", "test", ""),
			),
		)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	var sqsOpts []func(*sqs.Options)
	if cfg.UseLocalStack && cfg.AWSEndpoint != "" {
		sqsOpts = append(sqsOpts, func(o *sqs.Options) {
			o.BaseEndpoint = aws.String(cfg.AWSEndpoint)
		})
	}

	return &Client{
		sqs:  sqs.NewFromConfig(awsCfg, sqsOpts...),
		urls: make(map[string]string),
	}, nil
}

// QueueURL resolves a queue name to its URL, caching the result. Queue URLs are
// stable for the lifetime of the queue, so this is resolved once per name.
func (c *Client) QueueURL(ctx context.Context, name string) (string, error) {
	c.mu.RLock()
	url, ok := c.urls[name]
	c.mu.RUnlock()
	if ok {
		return url, nil
	}

	out, err := c.sqs.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{
		QueueName: aws.String(name),
	})
	if err != nil {
		return "", fmt.Errorf("get queue url %q: %w", name, err)
	}

	c.mu.Lock()
	c.urls[name] = *out.QueueUrl
	c.mu.Unlock()

	return *out.QueueUrl, nil
}

// Receive long-polls the named queue. It returns an empty slice (not an error)
// when the poll times out with nothing available, which is the common case.
func (c *Client) Receive(ctx context.Context, queueName string) ([]Message, error) {
	url, err := c.QueueURL(ctx, queueName)
	if err != nil {
		return nil, err
	}

	out, err := c.sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(url),
		MaxNumberOfMessages: maxMessages,
		WaitTimeSeconds:     longPollSeconds,
		// AttributeNames is deprecated in favour of MessageSystemAttributeNames,
		// but LocalStack 3.0 ignores the newer field and returns no system
		// attributes at all for it. That silently pinned every delivery's
		// receive count to the fallback of 1, so redeliveries logged and
		// published attempt=1 forever. Real SQS still honours this field; the
		// two are not set together because SQS rejects that combination.
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeName(sqstypes.MessageSystemAttributeNameApproximateReceiveCount),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("receive from %q: %w", queueName, err)
	}

	msgs := make([]Message, 0, len(out.Messages))
	for _, m := range out.Messages {
		msg := Message{
			ReceiveCount: 1,
		}
		if m.Body != nil {
			msg.Body = *m.Body
		}
		if m.ReceiptHandle != nil {
			msg.ReceiptHandle = *m.ReceiptHandle
		}
		if raw, ok := m.Attributes[string(sqstypes.MessageSystemAttributeNameApproximateReceiveCount)]; ok {
			if n, convErr := strconv.Atoi(raw); convErr == nil && n > 0 {
				msg.ReceiveCount = n
			}
		}
		msgs = append(msgs, msg)
	}

	return msgs, nil
}

// Delete removes a message from the queue. Deleting is what marks work as
// finished; a message that is not deleted becomes visible again when its
// visibility timeout expires, which is how transient failures get retried.
func (c *Client) Delete(ctx context.Context, queueName, receiptHandle string) error {
	url, err := c.QueueURL(ctx, queueName)
	if err != nil {
		return err
	}

	_, err = c.sqs.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(url),
		ReceiptHandle: aws.String(receiptHandle),
	})
	if err != nil {
		return fmt.Errorf("delete message from %q: %w", queueName, err)
	}
	return nil
}

// ExtendVisibility resets a message's visibility timeout to seconds from now.
//
// This is how a worker says "still working" on a job that outlives the queue's
// configured timeout. Without it, SQS decides the worker died, redelivers the
// message, and a second worker begins the same job — which the runner's
// idempotency guard cannot prevent, because that guard asks whether the output
// exists and the output does not exist until the work finishes.
func (c *Client) ExtendVisibility(ctx context.Context, queueName, receiptHandle string, seconds int32) error {
	url, err := c.QueueURL(ctx, queueName)
	if err != nil {
		return err
	}

	_, err = c.sqs.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(url),
		ReceiptHandle:     aws.String(receiptHandle),
		VisibilityTimeout: seconds,
	})
	if err != nil {
		return fmt.Errorf("extend visibility on %q: %w", queueName, err)
	}
	return nil
}

// Publish JSON-encodes body and sends it to the named queue.
func (c *Client) Publish(ctx context.Context, queueName string, body any) error {
	url, err := c.QueueURL(ctx, queueName)
	if err != nil {
		return err
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal message for %q: %w", queueName, err)
	}

	_, err = c.sqs.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(url),
		MessageBody: aws.String(string(payload)),
	})
	if err != nil {
		return fmt.Errorf("send message to %q: %w", queueName, err)
	}
	return nil
}
