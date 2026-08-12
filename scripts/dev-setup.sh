#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

echo "=========================================="
echo "DayReel Development Environment Setup"
echo "=========================================="
echo ""

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

print_status() {
    echo -e "${GREEN}[OK]${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

print_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Resource names. Keep in step with .env.example and
# backend/internal/events/messages.go.
RAW_BUCKET="${S3_RAW_BUCKET:-dayreel-raw-videos}"
PROCESSED_BUCKET="${S3_PROCESSED_BUCKET:-dayreel-processed}"
HLS_BUCKET="${S3_HLS_BUCKET:-dayreel-hls-output}"
JOBS_TABLE="${DYNAMODB_TABLE:-dayreel-jobs}"
QUEUE_DB="${QUEUE_DB_PATH:-./data/queue.db}"

# ==========================================================================
# Toolchain
# ==========================================================================

echo "Checking toolchain..."

if ! command -v go &> /dev/null; then
    print_error "Go is not installed. The API and worker run as plain 'go run' processes."
    exit 1
fi
print_status "Go is installed ($(go version | awk '{print $3}'))"

if ! command -v ffmpeg &> /dev/null; then
    print_error "ffmpeg is not installed. The pipeline workers cannot run without it."
    exit 1
fi
print_status "ffmpeg is installed"

if ! command -v ffprobe &> /dev/null; then
    print_error "ffprobe is not installed (usually ships with ffmpeg)."
    exit 1
fi
print_status "ffprobe is installed"

if ! command -v aws &> /dev/null; then
    print_error "AWS CLI is not installed. Required to verify buckets and the jobs table."
    exit 1
fi
print_status "AWS CLI is installed"

if ! command -v sqlite3 &> /dev/null; then
    print_warning "sqlite3 CLI is not installed. 'make queue-peek' will not work."
else
    print_status "sqlite3 CLI is installed"
fi

# ==========================================================================
# Environment file
# ==========================================================================

echo ""
echo "Setting up environment..."
if [ ! -f "$PROJECT_ROOT/.env" ]; then
    if [ -f "$PROJECT_ROOT/.env.example" ]; then
        cp "$PROJECT_ROOT/.env.example" "$PROJECT_ROOT/.env"
        print_status "Created .env from .env.example"
        print_warning "Fill in AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY before running anything."
    else
        print_error "No .env.example found — cannot create .env"
        exit 1
    fi
else
    print_status ".env file already exists"
fi

# The queue opens the SQLite file but does not create its parent directory, so a
# missing data/ is a startup failure rather than a first-run convenience.
mkdir -p "$PROJECT_ROOT/$(dirname "${QUEUE_DB#./}")"
print_status "Queue directory ready: $(dirname "$QUEUE_DB")"

# ==========================================================================
# AWS credentials
# ==========================================================================
# There is no emulator any more. Real credentials are required, and a wrong
# key shows up here rather than as a confusing 403 mid-upload.

echo ""
echo "Checking AWS credentials..."
if ! CALLER_ARN=$(aws sts get-caller-identity --query 'Arn' --output text 2>/dev/null); then
    print_error "aws sts get-caller-identity failed."
    print_error "Set real credentials in .env or ~/.aws/credentials, then re-run."
    exit 1
fi
print_status "Authenticated as $CALLER_ARN"

# ==========================================================================
# AWS resources
# ==========================================================================

echo ""
echo "Checking AWS resources..."
RESOURCES_OK=true

for BUCKET in "$RAW_BUCKET" "$PROCESSED_BUCKET" "$HLS_BUCKET"; do
    if aws s3api head-bucket --bucket "$BUCKET" &> /dev/null; then
        print_status "S3 bucket exists: $BUCKET"
    else
        print_error "S3 bucket missing or not accessible: $BUCKET"
        RESOURCES_OK=false
    fi
done

if aws dynamodb describe-table --table-name "$JOBS_TABLE" &> /dev/null; then
    print_status "DynamoDB table exists: $JOBS_TABLE"
else
    print_error "DynamoDB table missing or not accessible: $JOBS_TABLE"
    RESOURCES_OK=false
fi

if [ "$RESOURCES_OK" = false ]; then
    echo ""
    print_error "Some AWS resources are missing. Create them in your account before continuing:"
    echo "  aws s3 mb s3://$RAW_BUCKET"
    echo "  aws s3 mb s3://$PROCESSED_BUCKET"
    echo "  aws s3 mb s3://$HLS_BUCKET"
    echo "  aws dynamodb create-table --table-name $JOBS_TABLE \\"
    echo "    --attribute-definitions AttributeName=pk,AttributeType=S AttributeName=sk,AttributeType=S \\"
    echo "    --key-schema AttributeName=pk,KeyType=HASH AttributeName=sk,KeyType=RANGE \\"
    echo "    --billing-mode PAY_PER_REQUEST"
    echo ""
    print_error "The raw bucket also needs CORS for presigned uploads, and the HLS bucket"
    print_error "needs CORS plus player-readable access. ExposeHeaders must be [\"ETag\"] —"
    print_error "real S3 rejects wildcards there. See TROUBLESHOOTING.md."
    exit 1
fi

# ==========================================================================
# Status
# ==========================================================================

echo ""
echo "=========================================="
echo "Development environment is ready!"
echo "=========================================="
echo ""
echo "AWS resources (real account, no emulator):"
echo "  - S3 buckets: $RAW_BUCKET, $PROCESSED_BUCKET, $HLS_BUCKET"
echo "  - DynamoDB:   $JOBS_TABLE"
echo ""
echo "Local queue:"
echo "  - SQLite file: $QUEUE_DB (created on first run)"
echo ""
echo "Useful commands:"
echo "  make api                    - Run the API"
echo "  make worker STAGE=validate  - Run one pipeline stage"
echo "  make workers                - Run all four stages"
echo "  make queue-peek             - Dump the queue table"
echo "  make queue-reset            - Delete the queue file"
echo "  make verify                 - Re-check AWS credentials and resources"
echo ""
