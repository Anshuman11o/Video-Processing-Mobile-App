// Package db provides DynamoDB operations for job state management.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/anshumanagarwal/dayreel/internal/config"
	"github.com/anshumanagarwal/dayreel/internal/models"
)

// DynamoDBClient wraps the AWS DynamoDB client for job operations.
type DynamoDBClient struct {
	client *dynamodb.Client
	table  string
}

// NewDynamoDBClient creates a new DynamoDBClient connected to the configured table.
func NewDynamoDBClient(ctx context.Context, cfg *config.Config) (*DynamoDBClient, error) {
	var opts []func(*awsconfig.LoadOptions) error

	opts = append(opts, awsconfig.WithRegion(cfg.AWSRegion))

	if cfg.UseLocalStack && cfg.AWSEndpoint != "" {
		opts = append(opts,
			awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
		)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	var ddbOpts []func(*dynamodb.Options)
	if cfg.UseLocalStack && cfg.AWSEndpoint != "" {
		ddbOpts = append(ddbOpts, func(o *dynamodb.Options) {
			o.BaseEndpoint = aws.String(cfg.AWSEndpoint)
		})
	}

	client := dynamodb.NewFromConfig(awsCfg, ddbOpts...)

	return &DynamoDBClient{
		client: client,
		table:  cfg.DynamoDBTable,
	}, nil
}

// CreateJob inserts a new job record into DynamoDB.
func (d *DynamoDBClient) CreateJob(ctx context.Context, job *models.Job) error {
	item, err := attributevalue.MarshalMap(job)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}

	_, err = d.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(d.table),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("put job item: %w", err)
	}
	return nil
}

// GetJob retrieves a job by ID from DynamoDB.
func (d *DynamoDBClient) GetJob(ctx context.Context, jobID string) (*models.Job, error) {
	result, err := d.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(d.table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: "JOB#" + jobID},
			"sk": &ddbtypes.AttributeValueMemberS{Value: "METADATA"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get job item: %w", err)
	}
	if result.Item == nil {
		return nil, nil // Not found
	}

	var job models.Job
	if err := attributevalue.UnmarshalMap(result.Item, &job); err != nil {
		return nil, fmt.Errorf("unmarshal job: %w", err)
	}
	return &job, nil
}

// UpdateJobStatus updates the job's overall status and updated_at timestamp.
func (d *DynamoDBClient) UpdateJobStatus(ctx context.Context, jobID string, status models.JobStatus) error {
	now := time.Now().UTC()

	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: "JOB#" + jobID},
			"sk": &ddbtypes.AttributeValueMemberS{Value: "METADATA"},
		},
		UpdateExpression: aws.String("SET #status = :status, updated_at = :now"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":status": &ddbtypes.AttributeValueMemberS{Value: string(status)},
			":now":    &ddbtypes.AttributeValueMemberS{Value: now.Format(time.RFC3339Nano)},
		},
	})
	if err != nil {
		return fmt.Errorf("update job status: %w", err)
	}
	return nil
}

// UpdateUploadInfo sets the upload information on a job.
func (d *DynamoDBClient) UpdateUploadInfo(ctx context.Context, jobID string, upload *models.UploadInfo) error {
	uploadAV, err := attributevalue.MarshalMap(upload)
	if err != nil {
		return fmt.Errorf("marshal upload info: %w", err)
	}

	now := time.Now().UTC()

	_, err = d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: "JOB#" + jobID},
			"sk": &ddbtypes.AttributeValueMemberS{Value: "METADATA"},
		},
		UpdateExpression: aws.String("SET upload = :upload, #status = :status, updated_at = :now"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":upload": &ddbtypes.AttributeValueMemberM{Value: uploadAV},
			":status": &ddbtypes.AttributeValueMemberS{Value: string(models.JobStatusUploading)},
			":now":    &ddbtypes.AttributeValueMemberS{Value: now.Format(time.RFC3339Nano)},
		},
	})
	if err != nil {
		return fmt.Errorf("update upload info: %w", err)
	}
	return nil
}
