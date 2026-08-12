#!/bin/bash
set -euo pipefail

echo "============================================"
echo "Initializing DayReel AWS resources..."
echo "============================================"

# Configuration - must match backend/internal/events/messages.go constants
AWS_ENDPOINT="http://localhost:4566"
AWS_REGION="us-east-1"
ACCOUNT_ID="000000000000"

# ============================================================================
# S3 Buckets (must match BucketRawVideos, BucketProcessed, BucketHLSOutput)
# ============================================================================

echo ""
echo "Creating S3 buckets..."

awslocal s3 mb s3://dayreel-raw-videos || true
awslocal s3 mb s3://dayreel-processed || true
awslocal s3 mb s3://dayreel-hls-output || true

# Configure CORS for raw videos bucket (needed for presigned URL uploads from mobile)
echo "Configuring CORS..."
awslocal s3api put-bucket-cors --bucket dayreel-raw-videos --cors-configuration '{
  "CORSRules": [
    {
      "AllowedOrigins": ["*"],
      "AllowedMethods": ["GET", "PUT", "POST", "DELETE", "HEAD"],
      "AllowedHeaders": ["*"],
      "ExposeHeaders": ["ETag", "x-amz-meta-*"],
      "MaxAgeSeconds": 3600
    }
  ]
}'

# CORS for HLS output bucket (needed for browser/player access)
awslocal s3api put-bucket-cors --bucket dayreel-hls-output --cors-configuration '{
  "CORSRules": [
    {
      "AllowedOrigins": ["*"],
      "AllowedMethods": ["GET", "HEAD"],
      "AllowedHeaders": ["*"],
      "MaxAgeSeconds": 3600
    }
  ]
}'

echo "S3 buckets created."

# ============================================================================
# SQS Queues (must match QueueValidate, QueueExtract, QueueTranscribe, QueuePackage, QueueDLQ)
# ============================================================================

echo ""
echo "Creating SQS queues..."

# Create DLQ first (other queues reference it)
awslocal sqs create-queue --queue-name dayreel-dlq --attributes '{
  "MessageRetentionPeriod": "1209600",
  "VisibilityTimeout": "300"
}' || true

DLQ_ARN="arn:aws:sqs:${AWS_REGION}:${ACCOUNT_ID}:dayreel-dlq"

# Create worker queues with DLQ redrive policy (maxReceiveCount=3)
for QUEUE in dayreel-validate dayreel-extract dayreel-transcribe dayreel-package; do
  awslocal sqs create-queue --queue-name "$QUEUE" --attributes '{
    "VisibilityTimeout": "300",
    "MessageRetentionPeriod": "86400",
    "RedrivePolicy": "{\"deadLetterTargetArn\":\"'"${DLQ_ARN}"'\",\"maxReceiveCount\":\"3\"}"
  }' || true
  echo "  Created queue: $QUEUE"
done

echo "SQS queues created."

# ============================================================================
# DynamoDB Table (must match table name used in backend)
# ============================================================================

echo ""
echo "Creating DynamoDB table..."

awslocal dynamodb create-table \
  --table-name dayreel-jobs \
  --attribute-definitions \
    AttributeName=pk,AttributeType=S \
    AttributeName=sk,AttributeType=S \
  --key-schema \
    AttributeName=pk,KeyType=HASH \
    AttributeName=sk,KeyType=RANGE \
  --billing-mode PAY_PER_REQUEST \
  2>/dev/null || true

# Wait for table to be active
awslocal dynamodb wait table-exists --table-name dayreel-jobs 2>/dev/null || true

echo "DynamoDB table created."

# ============================================================================
# S3 Event Notifications (upload complete → validate queue)
# ============================================================================

echo ""
echo "Configuring S3 event notifications..."

VALIDATE_QUEUE_ARN="arn:aws:sqs:${AWS_REGION}:${ACCOUNT_ID}:dayreel-validate"

awslocal s3api put-bucket-notification-configuration \
  --bucket dayreel-raw-videos \
  --notification-configuration '{
    "QueueConfigurations": [
      {
        "QueueArn": "'"${VALIDATE_QUEUE_ARN}"'",
        "Events": ["s3:ObjectCreated:CompleteMultipartUpload", "s3:ObjectCreated:Put"],
        "Filter": {
          "Key": {
            "FilterRules": [
              {"Name": "suffix", "Value": ".mp4"}
            ]
          }
        }
      }
    ]
  }'

echo "S3 event notifications configured."

# ============================================================================
# Summary
# ============================================================================

echo ""
echo "============================================"
echo "DayReel AWS resources initialized!"
echo "============================================"
echo ""
echo "S3 Buckets:"
awslocal s3 ls
echo ""
echo "SQS Queues:"
awslocal sqs list-queues --query 'QueueUrls' --output table 2>/dev/null || awslocal sqs list-queues
echo ""
echo "DynamoDB Tables:"
awslocal dynamodb list-tables --query 'TableNames' --output table 2>/dev/null || awslocal dynamodb list-tables
echo ""
echo "Endpoints:"
echo "  S3/SQS/DynamoDB: http://localhost:4566"
echo "  Redis:           localhost:6379"
echo ""
