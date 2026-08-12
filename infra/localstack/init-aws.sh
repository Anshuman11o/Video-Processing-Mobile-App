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

# Public read on the HLS bucket.
#
# LOCAL DEVELOPMENT ONLY. HLS playlists reference their segments by relative
# path, so a presigned master playlist is followed by 403s on every segment —
# presigning cannot work for HLS without rewriting every URI, which defeats the
# format. A readable bucket is what CloudFront would sit in front of in
# production.
#
# The real-AWS access model is deliberately an OPEN QUESTION and must be decided
# before anything deploys. A public bucket on a real account is both a cost and
# an exposure; see config/free-tier.md.
awslocal s3api put-bucket-policy --bucket dayreel-hls-output --policy '{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "PublicReadLocalDevOnly",
      "Effect": "Allow",
      "Principal": "*",
      "Action": "s3:GetObject",
      "Resource": "arn:aws:s3:::dayreel-hls-output/*"
    }
  ]
}' || true

# Reap abandoned multipart uploads on the raw bucket.
#
# Every interrupted upload leaves its parts behind. They bill as storage from
# the moment the first part lands, they are released only by a completion or an
# abort, and they appear in NO object listing — not `aws s3 ls`, not the
# console. ListMultipartUploads is the only thing that can see them, which also
# means they cannot be named in the teardown reminder config/free-tier.md
# requires. That is the argument for this rule; the money is pennies.
#
# One day is the floor. The field is DaysAfterInitiation and it takes an
# integer, so there is no shorter rule to write.
#
# LOCALSTACK DOES NOT ENFORCE THIS. Verified 2026-08-13, not assumed:
#   - the PUT succeeds and get-bucket-lifecycle-configuration reads it back
#     verbatim, so "it applied" proves nothing;
#   - LocalStack does not validate it either — DaysAfterInitiation=0 is accepted
#     here and rejected by real S3;
#   - it parses the rule far enough to report a derived expiry-date on
#     HeadObject, which makes it look implemented;
#   - and it never acts. An object whose lifecycle Expiration Date was in 2020
#     was still present, and an incomplete multipart upload under a
#     DaysAfterInitiation=0 rule survived a minute of polling and repeated
#     bucket access.
#
# Same shape as the public-read policy below and the put-public-access-block
# precedent in docs/SETUP.md: accepted, readable, ignored. The rule is here for
# real AWS. Locally, scripts/abort-stale-uploads.sh is the thing that actually
# reaps — run it, do not trust this.
awslocal s3api put-bucket-lifecycle-configuration \
  --bucket dayreel-raw-videos \
  --lifecycle-configuration '{
    "Rules": [
      {
        "ID": "AbortIncompleteMultipartUploads",
        "Status": "Enabled",
        "Filter": {"Prefix": ""},
        "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 1}
      }
    ]
  }' || true

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
#
# Transcribe gets a longer visibility timeout than the rest. Every other stage
# finishes in about a second, but speech recognition runs for minutes on CPU. If
# the message becomes visible again while the first worker is still transcribing,
# SQS redelivers and a second worker starts the same job — and the runner's
# idempotency guard cannot catch that, because it checks whether the output
# exists and the output does not exist until the work finishes.
for QUEUE in dayreel-validate dayreel-extract dayreel-transcribe dayreel-package; do
  if [ "$QUEUE" = "dayreel-transcribe" ]; then
    VISIBILITY="900"
  else
    VISIBILITY="300"
  fi

  awslocal sqs create-queue --queue-name "$QUEUE" --attributes '{
    "VisibilityTimeout": "'"${VISIBILITY}"'",
    "MessageRetentionPeriod": "86400",
    "RedrivePolicy": "{\"deadLetterTargetArn\":\"'"${DLQ_ARN}"'\",\"maxReceiveCount\":\"3\"}"
  }' || true
  echo "  Created queue: $QUEUE (visibility ${VISIBILITY}s)"
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

# No key filter, deliberately. The API issues presigned URLs for any filename
# and writes the key as <job_id>/<original_filename>, so a suffix filter on
# ".mp4" silently dropped every .mov/.webm upload: the job completed its upload
# and then sat in "processing" forever with no error and no DLQ entry.
#
# The validate worker gates on content via ffprobe instead, which is what a
# stage named "validate" should be doing anyway. The cost is that any other
# object written to this bucket also enqueues a validate attempt; the worker
# rejects those permanently when the key has no UUID job-id prefix.
awslocal s3api put-bucket-notification-configuration \
  --bucket dayreel-raw-videos \
  --notification-configuration '{
    "QueueConfigurations": [
      {
        "QueueArn": "'"${VALIDATE_QUEUE_ARN}"'",
        "Events": ["s3:ObjectCreated:CompleteMultipartUpload", "s3:ObjectCreated:Put"]
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
