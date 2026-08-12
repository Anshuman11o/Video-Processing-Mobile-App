# DayReel Development Makefile
#
# The local stack is two Go processes and a SQLite file. There are no
# containers to start and no AWS emulator: S3 and DynamoDB calls go to a real
# AWS account, so `make verify` is the closest thing to "is my stack up?".
#
# The queue is the one pluggable piece. QUEUE_DRIVER=sqlite (the default) needs
# nothing but the file; QUEUE_DRIVER=sqs needs the queues to exist first, which
# is what the sqs-* targets below are for.

.PHONY: api worker workers queue-peek queue-reset sqs-setup sqs-status sqs-teardown verify test help

# Overridable so these stay in step with .env without duplicating it.
QUEUE_DB_PATH ?= data/queue.db
S3_RAW_BUCKET ?= dayreel-raw-videos
S3_PROCESSED_BUCKET ?= dayreel-processed
S3_HLS_BUCKET ?= dayreel-hls-output
DYNAMODB_TABLE ?= dayreel-jobs

# One worker binary serves every stage; WORKER_STAGE selects which.
STAGE ?= validate

# Compose used to inject the environment into each container. Nothing does that
# now, and the Go processes read plain environment variables, so the run targets
# source .env themselves — otherwise .env would be a file nothing reads.
LOAD_ENV = set -a; [ -f .env ] && . ./.env || true; set +a;

# Default target
help:
	@echo "DayReel Development Commands"
	@echo "============================"
	@echo ""
	@echo "Processes:"
	@echo "  make api          - Run the API (localhost:8080)"
	@echo "  make worker       - Run one pipeline stage (STAGE=validate by default)"
	@echo "  make workers      - Run all four stages in one terminal"
	@echo ""
	@echo "Queue (SQLite driver — the default):"
	@echo "  make queue-peek   - Dump the SQLite queue table"
	@echo "  make queue-reset  - Delete the SQLite queue file"
	@echo ""
	@echo "Queue (SQS driver — QUEUE_DRIVER=sqs):"
	@echo "  make sqs-setup    - Create the five queues in AWS (idempotent)"
	@echo "  make sqs-status   - Show each queue's URL, depth and in-flight count"
	@echo "  make sqs-teardown - Delete them (prompts for confirmation)"
	@echo ""
	@echo "Checks:"
	@echo "  make verify       - Check AWS credentials, buckets and table"
	@echo "  make test         - Run the Go test suite"
	@echo ""

# Run the API
api:
	@echo "Starting API..."
	@$(LOAD_ENV) cd backend && go run ./cmd/api

# Run one stage of the pipeline.
#   make worker STAGE=extract
# WORKER_STAGE is required by cmd/worker — a worker with no stage has nothing
# to poll for. Every stage shells out to ffmpeg/ffprobe, so they must be on PATH
# (scripts/dev-setup.sh checks this).
worker:
	@echo "Starting worker: stage=$(STAGE)"
	@$(LOAD_ENV) cd backend && WORKER_STAGE=$(STAGE) go run ./cmd/worker

# All four stages at once, which is what the four worker containers used to do.
# Output is interleaved and Ctrl-C stops the lot; run `make worker STAGE=...`
# in separate terminals when you need to read one stage's log on its own.
workers:
	@echo "Starting validate, extract, transcribe and package workers. Ctrl-C stops all."
	@$(LOAD_ENV) cd backend && \
		trap 'kill 0' INT TERM; \
		for s in validate extract transcribe package; do \
			WORKER_STAGE=$$s go run ./cmd/worker 2>&1 | sed "s/^/[$$s] /" & \
		done; \
		wait

# Dump the queue so you can watch messages move between stages.
# Columns must match the schema in backend/internal/queue/sqlite.go: this reads
# `messages(id, queue, receive_count, visible_at, receipt)` and treats
# `visible_at` as epoch milliseconds. Change the schema, change this query.
queue-peek:
	@test -f "$(QUEUE_DB_PATH)" || { echo "No queue database at $(QUEUE_DB_PATH). Start the API or worker first."; exit 1; }
	@command -v sqlite3 >/dev/null 2>&1 || { echo "sqlite3 CLI not installed (brew install sqlite / apt install sqlite3)."; exit 1; }
	@echo "Queue: $(QUEUE_DB_PATH)"
	@echo ""
	@sqlite3 -header -column "$(QUEUE_DB_PATH)" \
		"SELECT id, queue, receive_count, \
		        datetime(visible_at/1000, 'unixepoch') AS visible_at, \
		        CASE WHEN receipt IS NULL THEN '' ELSE 'leased' END AS state \
		   FROM messages ORDER BY queue, id;"
	@echo ""
	@echo "Depth by queue:"
	@sqlite3 -header -column "$(QUEUE_DB_PATH)" \
		"SELECT queue, COUNT(*) AS depth FROM messages GROUP BY queue ORDER BY queue;"

# Wipe the queue. The file is recreated on next start.
queue-reset:
	@rm -f "$(QUEUE_DB_PATH)" "$(QUEUE_DB_PATH)-wal" "$(QUEUE_DB_PATH)-shm"
	@echo "Removed $(QUEUE_DB_PATH)."

# The SQS driver's equivalents of the two targets above. They shell out rather
# than inline the aws calls: creating a queue set means a redrive policy that
# has to be built after the DLQ exists, which is a script, not a recipe line.
#
# The environment is loaded the same way the run targets load it, so the
# visibility timeout and delivery budget written onto the queues are the ones
# the workers will be started with. Setting them in two places is how a queue
# ends up enforcing a five minute lease against workers that believe they have
# ten.
sqs-setup:
	@$(LOAD_ENV) ./scripts/aws-sqs-setup.sh create

sqs-status:
	@$(LOAD_ENV) ./scripts/aws-sqs-setup.sh status

# Not chained into anything: it destroys messages, and it needs a human at the
# keyboard to confirm the region.
sqs-teardown:
	@$(LOAD_ENV) ./scripts/aws-sqs-setup.sh teardown

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
