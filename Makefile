# DayReel Development Makefile
#
# The local stack is two Go processes and a SQLite file. There are no
# containers to start and no AWS emulator: S3 and DynamoDB calls go to a real
# AWS account, so `make verify` is the closest thing to "is my stack up?".

.PHONY: api worker queue-peek queue-reset verify test help

# Overridable so these stay in step with .env without duplicating it.
QUEUE_DB_PATH ?= data/queue.db
S3_RAW_BUCKET ?= dayreel-raw-videos
S3_PROCESSED_BUCKET ?= dayreel-processed
S3_HLS_BUCKET ?= dayreel-hls-output
DYNAMODB_TABLE ?= dayreel-jobs

# Default target
help:
	@echo "DayReel Development Commands"
	@echo "============================"
	@echo ""
	@echo "Processes:"
	@echo "  make api          - Run the API (localhost:8080)"
	@echo "  make worker       - Run the pipeline worker"
	@echo ""
	@echo "Queue:"
	@echo "  make queue-peek   - Dump the SQLite queue table"
	@echo "  make queue-reset  - Delete the SQLite queue file"
	@echo ""
	@echo "Checks:"
	@echo "  make verify       - Check AWS credentials, buckets and table"
	@echo "  make test         - Run the Go test suite"
	@echo ""

# Run the API
api:
	@echo "Starting API..."
	@cd backend && go run ./cmd/api

# Run the worker.
# NOTE: backend/cmd/worker does not exist yet — it arrives with the first
# pipeline stage (Stage 3A, validate worker). Until then this target fails
# with "no Go files"; that is expected, not a broken Makefile.
worker:
	@echo "Starting worker..."
	@cd backend && go run ./cmd/worker

# Dump the queue so you can watch messages move between stages.
# Columns must match the schema in backend/internal/queue/.
queue-peek:
	@test -f "$(QUEUE_DB_PATH)" || { echo "No queue database at $(QUEUE_DB_PATH). Start the API or worker first."; exit 1; }
	@command -v sqlite3 >/dev/null 2>&1 || { echo "sqlite3 CLI not installed (brew install sqlite / apt install sqlite3)."; exit 1; }
	@echo "Queue: $(QUEUE_DB_PATH)"
	@echo ""
	@sqlite3 -header -column "$(QUEUE_DB_PATH)" \
		"SELECT id, queue, deliveries, visible_at, created_at FROM messages ORDER BY queue, id;"
	@echo ""
	@echo "Depth by queue:"
	@sqlite3 -header -column "$(QUEUE_DB_PATH)" \
		"SELECT queue, COUNT(*) AS depth FROM messages GROUP BY queue ORDER BY queue;"

# Wipe the queue. The file is recreated on next start.
queue-reset:
	@rm -f "$(QUEUE_DB_PATH)" "$(QUEUE_DB_PATH)-wal" "$(QUEUE_DB_PATH)-shm"
	@echo "Removed $(QUEUE_DB_PATH)."

# Confirm the real AWS resources this project needs actually exist.
verify:
	@echo "Verifying AWS access..."
	@echo ""
	@echo "=== Credentials ==="
	@aws sts get-caller-identity --output text --query 'Arn' || { echo "AWS credentials are not working. Check .env / ~/.aws/credentials."; exit 1; }
	@echo ""
	@echo "=== S3 Buckets ==="
	@for b in $(S3_RAW_BUCKET) $(S3_PROCESSED_BUCKET) $(S3_HLS_BUCKET); do \
		if aws s3api head-bucket --bucket "$$b" 2>/dev/null; then \
			echo "  OK      $$b"; \
		else \
			echo "  MISSING $$b"; \
		fi; \
	done
	@echo ""
	@echo "=== DynamoDB Table ==="
	@aws dynamodb describe-table --table-name $(DYNAMODB_TABLE) \
		--query 'Table.[TableName,TableStatus]' --output text 2>/dev/null \
		|| echo "  MISSING $(DYNAMODB_TABLE)"
	@echo ""
	@echo "Verification complete."

# Go test suite
test:
	@cd backend && go test ./...
