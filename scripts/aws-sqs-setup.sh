#!/usr/bin/env bash
#
# Create, inspect and tear down the five SQS queues the pipeline uses when
# QUEUE_DRIVER=sqs.
#
#   ./scripts/aws-sqs-setup.sh create     # idempotent; safe to re-run
#   ./scripts/aws-sqs-setup.sh status     # URL, depth and in-flight per queue
#   ./scripts/aws-sqs-setup.sh teardown   # delete them, after confirming
#
# This targets REAL AWS. There is no emulator any more (see infra/CONTEXT.md),
# and every queue created here is a billable resource: SQS bills per request,
# and an idle worker long-polling an empty queue still makes one request every
# 20 seconds per stage. The default driver is SQLite precisely so none of this
# has to exist; run this only when you actually want the hosted broker.
#
# THE REDRIVE POLICY IS A SAFETY NET, NOT THE MECHANISM.
# Each stage queue gets a redrive policy pointing at the DLQ with
# maxReceiveCount = QUEUE_MAX_DELIVERIES, and the application does NOT rely on
# it. backend/internal/worker/runner.go dead-letters explicitly — immediately for
# a permanent failure like a rejected codec, with the whole delivery budget
# unspent, because the remaining attempts would fail identically. Redrive only
# fires after maxReceiveCount deliveries, so it catches the cases that never
# reach that call at all: a worker killed mid-stage, a message whose body will
# not decode. Change one of the two and you have not changed the other.
#
# Environment (all optional):
#   AWS_REGION                default us-east-1
#   QUEUE_MAX_DELIVERIES      default 3    — must match the workers' config
#   QUEUE_VISIBILITY_TIMEOUT  default 300  — seconds; must match the workers'
#   QUEUE_RETENTION           default 345600 — seconds (4 days), SQS's own default
#   DLQ_RETENTION             default 1209600 — seconds (14 days), SQS's maximum
set -uo pipefail

# Colors for output. Same palette and helpers as scripts/dev-setup.sh, so the
# two read alike when they are run back to back.
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

# Queue names. Keep in step with backend/internal/events/messages.go and
# .env.example — the workers resolve these names to URLs at startup, so a name
# that differs by one character is a worker that polls a queue nobody writes to.
DLQ_NAME="${QUEUE_DLQ:-dayreel-dlq}"
STAGE_QUEUES=(
    "${QUEUE_VALIDATE:-dayreel-validate}"
    "${QUEUE_EXTRACT:-dayreel-extract}"
    "${QUEUE_TRANSCRIBE:-dayreel-transcribe}"
    "${QUEUE_PACKAGE:-dayreel-package}"
)

REGION="${AWS_REGION:-us-east-1}"
MAX_DELIVERIES="${QUEUE_MAX_DELIVERIES:-3}"
RETENTION="${QUEUE_RETENTION:-345600}"
DLQ_RETENTION="${DLQ_RETENTION:-1209600}"

# QUEUE_VISIBILITY_TIMEOUT is a Go duration in .env ("5m") and a plain number of
# seconds to SQS. Accept both so one variable can configure both sides.
raw_visibility="${QUEUE_VISIBILITY_TIMEOUT:-300}"
case "$raw_visibility" in
    *m) VISIBILITY=$(( ${raw_visibility%m} * 60 )) ;;
    *s) VISIBILITY="${raw_visibility%s}" ;;
    *h) VISIBILITY=$(( ${raw_visibility%h} * 3600 )) ;;
    *)  VISIBILITY="$raw_visibility" ;;
esac

# ==========================================================================
# Guards
# ==========================================================================

# AWS_ENDPOINT_URL redirects every AWS CLI call at a local emulator. There is no
# emulator in this project any more, so a set value means either a stale shell
# from the LocalStack era or a mistake — and both produce the same failure:
# queues created somewhere the workers will never look, while `status` cheerfully
# reports them as present. Refuse rather than half-work.
if [ -n "${AWS_ENDPOINT_URL:-}" ]; then
    print_error "AWS_ENDPOINT_URL is set to '$AWS_ENDPOINT_URL'."
    print_error "This script targets real AWS only. Unset it and re-run:"
    print_error "  unset AWS_ENDPOINT_URL"
    exit 1
fi

# Checked per command rather than at load time, so --help works on a machine
# with no credentials configured — which is exactly the machine whose owner is
# reading the help.
CALLER_ARN=""
require_aws() {
    if ! command -v aws &> /dev/null; then
        print_error "AWS CLI is not installed."
        exit 1
    fi
    # Credentials before resources: a bad key otherwise shows up as five
    # missing queues, which is a misleading diagnosis.
    if ! CALLER_ARN=$(aws sts get-caller-identity --query 'Arn' --output text 2>/dev/null); then
        print_error "aws sts get-caller-identity failed."
        print_error "Set real credentials in .env or ~/.aws/credentials, then re-run."
        exit 1
    fi
}

sqs() {
    aws sqs --region "$REGION" "$@"
}

# queue_url NAME — the URL, or empty if the queue does not exist.
queue_url() {
    sqs get-queue-url --queue-name "$1" --query 'QueueUrl' --output text 2>/dev/null
}

# attrs_json builds the --attributes argument as JSON rather than the CLI's
# Key=Value,Key=Value shorthand.
#
# The shorthand cannot express this: a RedrivePolicy is itself a JSON document
# containing a comma, and the shorthand parser splits on commas, so
# `RedrivePolicy={"deadLetterTargetArn":"...","maxReceiveCount":"3"}` is read as
# two malformed attributes. The queue is then created with no redrive policy at
# all, or the call fails with an unhelpful parse error.
#
# SQS's attribute map is map[string]string, so the policy goes in as an escaped
# JSON *string*, not as a nested object.
#
#   attrs_json REDRIVE_POLICY_OR_EMPTY RETENTION_SECONDS
attrs_json() {
    local redrive="$1" retention="$2"
    local base="\"VisibilityTimeout\":\"$VISIBILITY\",\"MessageRetentionPeriod\":\"$retention\""
    if [ -z "$redrive" ]; then
        printf '{%s}' "$base"
        return
    fi
    printf '{%s,"RedrivePolicy":"%s"}' "$base" "${redrive//\"/\\\"}"
}

# ==========================================================================
# create
# ==========================================================================

cmd_create() {
    require_aws

    echo "=========================================="
    echo "DayReel SQS Queues — create"
    echo "=========================================="
    echo ""
    echo "Account:            $CALLER_ARN"
    echo "Region:             $REGION"
    echo "Visibility timeout: ${VISIBILITY}s"
    echo "Max deliveries:     $MAX_DELIVERIES (redrive safety net)"
    echo ""

    # The DLQ first, unconditionally: a stage queue's redrive policy names the
    # DLQ by ARN, so it cannot be created before the DLQ exists.
    local dlq_url dlq_arn
    dlq_url=$(queue_url "$DLQ_NAME")
    if [ -n "$dlq_url" ]; then
        print_status "DLQ already exists: $DLQ_NAME"
    else
        # No redrive policy on the DLQ itself: it is where messages stop.
        dlq_url=$(sqs create-queue --queue-name "$DLQ_NAME" \
            --attributes "$(attrs_json "" "$DLQ_RETENTION")" \
            --query 'QueueUrl' --output text) || {
            print_error "Failed to create $DLQ_NAME"
            exit 1
        }
        # Longer retention than the stage queues on purpose: a dead letter is
        # only useful if someone can still read it days later, and nothing
        # drains this queue automatically.
        print_status "Created DLQ: $DLQ_NAME (retention ${DLQ_RETENTION}s)"
    fi

    dlq_arn=$(sqs get-queue-attributes --queue-url "$dlq_url" \
        --attribute-names QueueArn --query 'Attributes.QueueArn' --output text) || {
        print_error "Could not read the DLQ ARN; cannot attach redrive policies."
        exit 1
    }

    local redrive stage_attrs
    redrive="{\"deadLetterTargetArn\":\"$dlq_arn\",\"maxReceiveCount\":\"$MAX_DELIVERIES\"}"
    stage_attrs="$(attrs_json "$redrive" "$RETENTION")"

    for q in "${STAGE_QUEUES[@]}"; do
        local url
        url=$(queue_url "$q")
        if [ -n "$url" ]; then
            # Idempotent, and not merely "skip if present": set-queue-attributes
            # is what makes a re-run after changing QUEUE_VISIBILITY_TIMEOUT
            # actually converge. A queue whose attributes silently disagree with
            # the workers' config is the failure this avoids — the workers
            # compute lease deadlines from the queue's attribute, and AWS
            # enforces it.
            sqs set-queue-attributes --queue-url "$url" \
                --attributes "$stage_attrs" >/dev/null || {
                print_error "Failed to update attributes on $q"
                exit 1
            }
            print_status "Updated: $q"
        else
            sqs create-queue --queue-name "$q" \
                --attributes "$stage_attrs" \
                --query 'QueueUrl' --output text >/dev/null || {
                print_error "Failed to create $q"
                exit 1
            }
            print_status "Created: $q"
        fi
    done

    echo ""
    print_warning "These five queues are billable resources in $REGION:"
    printf '  %s\n' "$DLQ_NAME" "${STAGE_QUEUES[@]}"
    print_warning "Run './scripts/aws-sqs-setup.sh teardown' when you are done with them."
    echo ""
    echo "To use them, set in .env:"
    echo "  QUEUE_DRIVER=sqs"
    echo "  QUEUE_VISIBILITY_TIMEOUT=${VISIBILITY}s"
    echo "  QUEUE_MAX_DELIVERIES=$MAX_DELIVERIES"
}

# ==========================================================================
# status
# ==========================================================================

cmd_status() {
    require_aws

    echo "DayReel SQS queues — region $REGION"
    echo ""
    printf '%-22s %10s %10s  %s\n' "QUEUE" "VISIBLE" "IN-FLIGHT" "URL"

    local missing=0
    for q in "${STAGE_QUEUES[@]}" "$DLQ_NAME"; do
        local url
        url=$(queue_url "$q")
        if [ -z "$url" ]; then
            printf '%-22s %10s %10s  %s\n' "$q" "-" "-" "MISSING"
            missing=1
            continue
        fi

        # Both counts are approximate, and SQS documents them as such. In-flight
        # is the one worth watching: a queue with depth 0 and a stuck in-flight
        # count is a worker holding a lease it will never ack.
        local attrs visible inflight
        attrs=$(sqs get-queue-attributes --queue-url "$url" \
            --attribute-names ApproximateNumberOfMessages ApproximateNumberOfMessagesNotVisible \
            --query 'Attributes.[ApproximateNumberOfMessages,ApproximateNumberOfMessagesNotVisible]' \
            --output text 2>/dev/null)
        visible=$(echo "$attrs" | awk '{print $1}')
        inflight=$(echo "$attrs" | awk '{print $2}')
        printf '%-22s %10s %10s  %s\n' "$q" "${visible:-?}" "${inflight:-?}" "$url"
    done

    if [ "$missing" -eq 1 ]; then
        echo ""
        print_warning "Some queues are missing. Run './scripts/aws-sqs-setup.sh create'."
        return 1
    fi
}

# ==========================================================================
# teardown
# ==========================================================================

cmd_teardown() {
    require_aws

    echo "About to DELETE these queues in $REGION:"
    printf '  %s\n' "${STAGE_QUEUES[@]}" "$DLQ_NAME"
    echo ""
    print_warning "Any messages still on them are destroyed. Jobs mid-pipeline will never finish."
    echo ""

    # A prompt, not a --yes flag. A flag is something you paste from your shell
    # history at the wrong moment; typing the region back means reading the line
    # above it first.
    printf "Type the region (%s) to confirm: " "$REGION"
    read -r answer
    if [ "$answer" != "$REGION" ]; then
        echo "Aborted. Nothing was deleted."
        exit 1
    fi

    for q in "${STAGE_QUEUES[@]}" "$DLQ_NAME"; do
        local url
        url=$(queue_url "$q")
        if [ -z "$url" ]; then
            print_warning "Not found, skipping: $q"
            continue
        fi
        if sqs delete-queue --queue-url "$url"; then
            print_status "Deleted: $q"
        else
            print_error "Failed to delete: $q"
        fi
    done

    echo ""
    # This one has bitten every project that scripts a teardown/create cycle.
    print_warning "SQS refuses to recreate a queue with the same name for 60 seconds"
    print_warning "after deletion (QueueDeletedRecently). Wait a minute before running"
    print_warning "'create' again, or it will fail on the first queue and leave the rest"
    print_warning "of the set missing."
}

# ==========================================================================

case "${1:-}" in
    create)   cmd_create ;;
    status)   cmd_status ;;
    teardown) cmd_teardown ;;
    -h|--help|"")
        sed -n '2,30p' "$0" | sed 's/^#\ \?//'
        ;;
    *)
        print_error "unknown command: $1"
        echo "Usage: $0 {create|status|teardown}" >&2
        exit 2
        ;;
esac
