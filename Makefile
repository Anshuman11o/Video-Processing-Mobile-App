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

# Fallbacks only. LOAD_ENV below sources .env into each recipe's shell, so a
# name set there wins over these; they exist so the targets still do something
# sensible before .env is created.
#
# Recipes read them as shell variables ("$${VAR:-$(VAR)}") rather than as make
# variables ("$(VAR)"), which is what lets .env win. A bare $(VAR) expands at
# parse time, before LOAD_ENV has run, baking the default in — that was the bug
# where `make queue-peek` reported an empty queue while the pipeline drained one.
#
# Precedence is .env > environment > these defaults. Note the middle one: `set
# -a; . ./.env` overwrites variables already exported, so `S3_RAW_BUCKET=x make
# verify` loses to a value in .env rather than overriding it. That has always
# been how the run targets behave; it is now how all of them behave. Edit .env,
# or comment the line out there, to change what these check.
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
#
# The prefix is a `read` loop and not `sed`, because sed block-buffers when its
# stdout is not a terminal. `make workers > run.log` therefore printed the echo
# above and then NOTHING, indefinitely, while four workers ran perfectly and
# their output sat in 4 KiB of sed buffer — unobservable in exactly the case you
# redirect for. There is no portable sed flag for it: -u is GNU, -l is BSD, and
# this repo is developed on macOS and runs on Linux. The loop needs neither.
workers:
	@echo "Starting validate, extract, transcribe and package workers. Ctrl-C stops all."
	@$(LOAD_ENV) cd backend && \
		trap 'kill 0' INT TERM; \
		for s in validate extract transcribe package; do \
			WORKER_STAGE=$$s go run ./cmd/worker 2>&1 \
				| while IFS= read -r line; do printf '[%s] %s\n' "$$s" "$$line"; done & \
		done; \
		wait

# Dump the queue so you can watch messages move between stages.
# Columns must match the schema in backend/internal/queue/sqlite.go: this reads
# `messages(id, queue, receive_count, visible_at, receipt)` and treats
# `visible_at` as epoch milliseconds. Change the schema, change this query.
# QUEUE_DB_PATH is resolved the same way the API resolves it, which is the whole
# point of sourcing .env here: the API is started by `make api`, which does
# `cd backend` first, so a relative path in .env lands under backend/. This
# looked in the project root instead and reported an empty queue while the
# pipeline was visibly draining one.
queue-peek:
	@$(LOAD_ENV) db="$${QUEUE_DB_PATH:-$(QUEUE_DB_PATH)}"; \
	case "$$db" in /*) ;; *) db="backend/$${db#./}" ;; esac; \
	test -f "$$db" || { echo "No queue database at $$db. Start the API or worker first."; exit 1; }; \
	command -v sqlite3 >/dev/null 2>&1 || { echo "sqlite3 CLI not installed (brew install sqlite / apt install sqlite3)."; exit 1; }; \
	echo "Queue: $$db"; \
	echo ""; \
	sqlite3 -header -column "$$db" \
		"SELECT id, queue, receive_count, \
		        datetime(visible_at/1000, 'unixepoch') AS visible_at, \
		        CASE WHEN receipt IS NULL THEN '' ELSE 'leased' END AS state \
		   FROM messages ORDER BY queue, id;"; \
	echo ""; \
	echo "Depth by queue:"; \
	sqlite3 -header -column "$$db" \
		"SELECT queue, COUNT(*) AS depth FROM messages GROUP BY queue ORDER BY queue;"

# Wipe the queue. The file is recreated on next start.
queue-reset:
	@$(LOAD_ENV) db="$${QUEUE_DB_PATH:-$(QUEUE_DB_PATH)}"; \
	case "$$db" in /*) ;; *) db="backend/$${db#./}" ;; esac; \
	rm -f "$$db" "$$db-wal" "$$db-shm"; \
	echo "Removed $$db."

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
# Sources .env for the same reason queue-peek does: bucket names are globally
# unique, so the defaults below are names somebody else owns. Checking those
# instead of yours reports three missing buckets on an account where all three
# exist.
verify:
	@$(LOAD_ENV) \
	echo "Verifying AWS access..."; \
	echo ""; \
	echo "=== Credentials ==="; \
	aws sts get-caller-identity --output text --query 'Arn' || { echo "AWS credentials are not working. Check .env / ~/.aws/credentials."; exit 1; }; \
	echo ""; \
	echo "=== S3 Buckets ==="; \
	for b in "$${S3_RAW_BUCKET:-$(S3_RAW_BUCKET)}" "$${S3_PROCESSED_BUCKET:-$(S3_PROCESSED_BUCKET)}" "$${S3_HLS_BUCKET:-$(S3_HLS_BUCKET)}"; do \
		if aws s3api head-bucket --bucket "$$b" 2>/dev/null; then \
			echo "  OK      $$b"; \
		else \
			echo "  MISSING $$b"; \
		fi; \
	done; \
	echo ""; \
	echo "=== DynamoDB Table ==="; \
	t="$${DYNAMODB_TABLE:-$(DYNAMODB_TABLE)}"; \
	aws dynamodb describe-table --table-name "$$t" \
		--query 'Table.[TableName,TableStatus]' --output text 2>/dev/null \
		|| echo "  MISSING $$t"; \
	echo ""; \
	echo "Verification complete."

# Go test suite
test:
	@cd backend && go test ./...
