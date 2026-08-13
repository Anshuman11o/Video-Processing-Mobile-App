#!/usr/bin/env bash
#
# List, and optionally abort, the multipart uploads that were started and never
# finished.
#
# These are the only S3 state in this project that costs money and appears in no
# listing anybody runs. `aws s3 ls` does not show them; neither does the
# console's object list. They exist from CreateMultipartUpload until a
# completion or an abort, and each held part bills as storage the whole time.
#
# That invisibility is the real problem, not the cents. config/free-tier.md
# requires that whatever brings up a billable resource NAME it at the end of the
# run — and a resource no listing shows cannot be named. This script is what
# makes them nameable.
#
#   ./scripts/abort-stale-uploads.sh                 # list only. Changes nothing.
#   ./scripts/abort-stale-uploads.sh --abort         # abort everything listed
#   ./scripts/abort-stale-uploads.sh --older-than 60 # only those over 60 min old
#   BUCKET=other ./scripts/abort-stale-uploads.sh --abort
#   TARGET=aws ./scripts/abort-stale-uploads.sh      # against real S3
#
# Listing is the default and aborting is opt-in, because an upload in this list
# may be one a client is still working through — age is the only signal
# separating "abandoned" from "in progress", and it is a guess.
#
# WHAT THIS DOES NOT PROVE: that the lifecycle rule in
# infra/localstack/init-aws.sh works. It does not, locally. LocalStack accepts
# the rule, reads it back verbatim, and never acts on it — verified 2026-08-13
# by leaving an incomplete upload under a DaysAfterInitiation=0 rule and
# watching it survive, and by leaving an object whose Expiration Date was in
# 2020 and watching that survive too. This script is the mechanism that
# genuinely reaps here. On real AWS the lifecycle rule is the safety net for the
# case this script cannot cover: nobody ever runs it.
set -uo pipefail

BUCKET="${BUCKET:-dayreel-raw-videos}"
TARGET="${TARGET:-localstack}"
CONTAINER="${CONTAINER:-dayreel-localstack}"

abort=0
older_than_min=0

while [ $# -gt 0 ]; do
  case "$1" in
    --abort)      abort=1 ;;
    --older-than) older_than_min="${2:?--older-than needs minutes}"; shift ;;
    -h|--help)    sed -n '2,35p' "$0" | sed 's/^#\ \?//'; exit 0 ;;
    *)            echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done

# One indirection for both targets so the commands below read the same either
# way. awslocal inside the container avoids needing credentials or an endpoint
# override on the host.
s3api() {
  if [ "$TARGET" = "aws" ]; then
    aws s3api "$@"
  else
    docker exec "$CONTAINER" awslocal s3api "$@"
  fi
}

echo "in-flight multipart uploads — bucket=$BUCKET target=$TARGET"

listing="$(s3api list-multipart-uploads --bucket "$BUCKET" --output json 2>/dev/null)"
if [ -z "$listing" ]; then
  echo "  could not list uploads (is the stack up? is the bucket right?)" >&2
  exit 1
fi

# python3 rather than jq: jq is not a documented prerequisite of this repo, and
# python3 is already relied on by scripts/verify-presign.sh.
rows="$(python3 -c '
import datetime, json, sys

listing = json.loads(sys.argv[1] or "{}")
threshold_min = int(sys.argv[2])
now = datetime.datetime.now(datetime.timezone.utc)

for u in listing.get("Uploads", []):
    initiated = u.get("Initiated")
    started = datetime.datetime.fromisoformat(initiated.replace("Z", "+00:00"))
    age_min = (now - started).total_seconds() / 60
    if age_min < threshold_min:
        continue
    # Tab separated: keys and upload ids contain no tabs, and upload ids are
    # long enough that a space-separated format is unreadable.
    print("%s\t%s\t%.0f" % (u["Key"], u["UploadId"], age_min))
' "$listing" "$older_than_min")"

if [ -z "$rows" ]; then
  if [ "$older_than_min" -gt 0 ]; then
    echo "  none older than ${older_than_min}m"
  else
    echo "  none"
  fi
  echo
  echo "Nothing is holding parts. This is the state a session should end in."
  exit 0
fi

count="$(printf '%s\n' "$rows" | wc -l | tr -d ' ')"

# Named before anything is destroyed, and named even in --abort mode, because
# the naming is the point.
printf '\n  %-56s %8s\n' "KEY" "AGE"
while IFS=$'\t' read -r key upload_id age; do
  printf '  %-56s %6sm\n' "$key" "$age"
done <<<"$rows"
printf '\n  %s upload(s) holding parts.\n\n' "$count"

if [ "$abort" -eq 0 ]; then
  echo "Listed only — nothing was aborted. Re-run with --abort to release these."
  echo "An upload here may still be in progress; --older-than is the only guard."
  exit 0
fi

failed=0
while IFS=$'\t' read -r key upload_id age; do
  if s3api abort-multipart-upload --bucket "$BUCKET" --key "$key" --upload-id "$upload_id" >/dev/null 2>&1; then
    echo "  aborted  $key"
  else
    # Not fatal: a concurrent completion or a second run makes this a no-op, and
    # the verification below is what actually decides whether the job is done.
    echo "  FAILED   $key" >&2
    failed=$((failed+1))
  fi
done <<<"$rows"

# Re-list rather than trusting the abort calls' exit codes. This project has
# been bitten repeatedly by operations that report success and change nothing.
#
# Counted in python rather than with --query 'length(Uploads)', which is a trap:
# when nothing is left, S3 omits Uploads entirely and jmespath's length() fails
# on the null with exit 255. The success case was the one that broke the check.
remaining="$(s3api list-multipart-uploads --bucket "$BUCKET" --output json 2>/dev/null \
  | python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("Uploads") or []))' 2>/dev/null)"
printf '\n  remaining in-flight uploads after abort: %s\n' "${remaining:-unknown}"

[ "$failed" -eq 0 ] && [ "${remaining:-none}" = "0" ]
