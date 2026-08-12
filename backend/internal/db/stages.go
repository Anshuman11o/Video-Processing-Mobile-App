package db

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/anshumanagarwal/dayreel/internal/models"
)

// Per-stage updates to the stages map on a job item.
//
// These update nested map paths (stages.<name>.<field>), so the stage name is
// an expression-attribute name rather than a literal. That only works because
// models.NewJob pre-seeds an entry for every stage: a SET against a path under
// a missing map key fails with ValidationException. If job creation ever stops
// seeding stages, these break.

func (d *DynamoDBClient) jobKey(jobID string) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		"pk": &ddbtypes.AttributeValueMemberS{Value: "JOB#" + jobID},
		"sk": &ddbtypes.AttributeValueMemberS{Value: "METADATA"},
	}
}

// SetStageRunning marks a stage running and increments its attempt counter.
//
// attempts uses ADD rather than a read-modify-write so that a redelivery
// racing the original cannot lose an increment.
func (d *DynamoDBClient) SetStageRunning(ctx context.Context, jobID string, stage models.StageName) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)

	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.table),
		Key:       d.jobKey(jobID),
		UpdateExpression: aws.String(
			"SET stages.#stg.#st = :st, stages.#stg.started_at = :now, updated_at = :now " +
				"ADD stages.#stg.attempts :one",
		),
		ExpressionAttributeNames: map[string]string{
			"#stg": string(stage),
			"#st":  "status",
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":st":  &ddbtypes.AttributeValueMemberS{Value: string(models.StageStatusRunning)},
			":now": &ddbtypes.AttributeValueMemberS{Value: now},
			":one": &ddbtypes.AttributeValueMemberN{Value: "1"},
		},
	})
	if err != nil {
		return fmt.Errorf("set stage %s running: %w", stage, err)
	}
	return nil
}

// SetStageCompleted marks a stage completed and records its output key.
func (d *DynamoDBClient) SetStageCompleted(ctx context.Context, jobID string, stage models.StageName, outputKey string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)

	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.table),
		Key:       d.jobKey(jobID),
		UpdateExpression: aws.String(
			"SET stages.#stg.#st = :st, stages.#stg.completed_at = :now, " +
				"stages.#stg.output_key = :key, stages.#stg.#err = :empty, updated_at = :now",
		),
		ExpressionAttributeNames: map[string]string{
			"#stg": string(stage),
			"#st":  "status",
			"#err": "error",
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":st":    &ddbtypes.AttributeValueMemberS{Value: string(models.StageStatusCompleted)},
			":now":   &ddbtypes.AttributeValueMemberS{Value: now},
			":key":   &ddbtypes.AttributeValueMemberS{Value: outputKey},
			":empty": &ddbtypes.AttributeValueMemberS{Value: ""},
		},
	})
	if err != nil {
		return fmt.Errorf("set stage %s completed: %w", stage, err)
	}
	return nil
}

// SetStageFailed marks a stage failed and flips the whole job to failed.
//
// Both happen in one UpdateItem: a job whose stage failed but whose top-level
// status still reads "processing" would look alive to every consumer of the
// API, which is worse than either state alone.
func (d *DynamoDBClient) SetStageFailed(ctx context.Context, jobID string, stage models.StageName, errMsg string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)

	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.table),
		Key:       d.jobKey(jobID),
		UpdateExpression: aws.String(
			"SET stages.#stg.#st = :stageFailed, stages.#stg.completed_at = :now, " +
				"stages.#stg.#err = :err, #jobst = :jobFailed, updated_at = :now",
		),
		ExpressionAttributeNames: map[string]string{
			"#stg":   string(stage),
			"#st":    "status",
			"#err":   "error",
			"#jobst": "status",
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":stageFailed": &ddbtypes.AttributeValueMemberS{Value: string(models.StageStatusFailed)},
			":jobFailed":   &ddbtypes.AttributeValueMemberS{Value: string(models.JobStatusFailed)},
			":err":         &ddbtypes.AttributeValueMemberS{Value: errMsg},
			":now":         &ddbtypes.AttributeValueMemberS{Value: now},
		},
	})
	if err != nil {
		return fmt.Errorf("set stage %s failed: %w", stage, err)
	}
	return nil
}

// stageDurationField maps a stage to its metrics attribute.
//
// models.Metrics uses one named field per stage rather than a map, so writing a
// duration means naming the attribute explicitly.
func stageDurationField(stage models.StageName) string {
	switch stage {
	case models.StageValidate:
		return "validate_duration_ms"
	case models.StageExtract:
		return "extract_duration_ms"
	case models.StageTranscribe:
		return "transcribe_duration_ms"
	case models.StagePackage:
		return "package_duration_ms"
	default:
		return ""
	}
}

// SetStageDuration records how long a stage's work took.
//
// Best-effort by design: callers log a failure and carry on, because losing a
// metric must never fail a job whose work actually succeeded. An unknown stage
// is a no-op rather than an error for the same reason.
func (d *DynamoDBClient) SetStageDuration(
	ctx context.Context, jobID string, stage models.StageName, ms int64,
) error {
	field := stageDurationField(stage)
	if field == "" {
		return nil
	}

	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(d.table),
		Key:              d.jobKey(jobID),
		UpdateExpression: aws.String("SET #metrics.#field = :ms"),
		ExpressionAttributeNames: map[string]string{
			// Both segments need aliasing: "metrics" is a DynamoDB reserved
			// keyword, and the per-stage field name is built at runtime.
			"#metrics": "metrics",
			"#field":   field,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":ms": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(ms, 10)},
		},
	})
	if err != nil {
		return fmt.Errorf("set stage duration: %w", err)
	}
	return nil
}
