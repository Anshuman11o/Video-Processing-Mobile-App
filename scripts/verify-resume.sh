#!/usr/bin/env bash
#
# Proves the stage 8A resume path from curl, with no app.
#
# The claim under test is narrow and worth stating exactly: an upload can be
# interrupted with some parts on S3 and finished later, using URLs the server
# mints fresh, without the client having recorded a single ETag. Everything else
# here is a negative check on that claim.
#
#   ./scripts/verify-resume.sh
#   API=http://localhost:8080 ./scripts/verify-resume.sh
#
# It talks to real S3 and a locally running API. Start `make api` and the four
# workers first — the last two checks run the pipeline end to end.
#
# THIS SCRIPT USED TO RUN AGAINST CONTAINERS AND COULD NOT RUN AT ALL. It shelled
# out to `docker exec dayreel-localstack awslocal`, generated its fixture inside
# `dayreel-worker-validate`, `docker cp`'d parts in and out, and redirected every
# PUT with `--connect-to 10.0.2.2:4566`. None of those exist now. What is left is
# simpler rather than more complex: ffmpeg is a documented host dependency
# (`scripts/dev-setup.sh` fails without it), the parts never leave the host, and
# a presigned URL for real S3 is reachable from wherever it was minted.
#
# ONE WORKAROUND SURVIVES, and it is not emulator-specific: `curl --data-binary`
# defaults to Content-Type: application/x-www-form-urlencoded. Every PUT here
# sets application/octet-stream instead. Under the emulator the wrong type
# produced a form-parse of a 5 MiB body and a 413 wrapped in a 500;
# scripts/verify-presign.sh never hit it because its payload is 29 bytes. Real
# S3 stores the part either way, but it also stores the content type, so sending
# the honest one is still right.
#
# Parts are 5 MiB minimum — S3 enforces it on every part but the last — so the
# fixture has to exceed 5 MiB for any of this to be multipart at all.
#
# It has to be a REAL video, not random bytes, because the last check runs the
# pipeline on the resumed upload and validate rejects anything ffprobe cannot
# read. The fixture is generated per run and never committed.
#
# IT COSTS MONEY. It creates four jobs, uploads ~23 MiB of parts, and runs the
# pipeline once. It aborts what it can and names what it leaves; read
# config/free-tier.md first.
set -uo pipefail

API="${API:-http://localhost:8080}"
BUCKET="${BUCKET:-${S3_RAW_BUCKET:-dayreel-raw-videos}}"

PART_SIZE=5242880

for dep in ffmpeg curl python3 aws; do
  command -v "$dep" >/dev/null 2>&1 || { echo "missing dependency: $dep" >&2; exit 1; }
done

# The host the API is expected to sign against, derived the same way the API
# derives it rather than hardcoded. This used to be the literal string
# "10.0.2.2:4566" in two assertions, which is why they could only ever have
# passed under the emulator.
#
# Empty S3_PUBLIC_ENDPOINT means the SDK's own regional endpoint, which is
# virtual-hosted style: the bucket is part of the host, not a path segment.
REGION="${AWS_REGION:-$(aws configure get region 2>/dev/null || echo us-east-1)}"
if [ -n "${S3_PUBLIC_ENDPOINT:-}" ]; then
  EXPECTED_HOST="$(python3 -c 'import sys,urllib.parse as u; print(u.urlparse(sys.argv[1]).netloc)' "$S3_PUBLIC_ENDPOINT")"
else
  EXPECTED_HOST="$BUCKET.s3.$REGION.amazonaws.com"
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

pass=0; fail=0

check() {
  local name="$1" want="$2" got="$3"
  if [ "$got" = "$want" ]; then
    printf '  %-46s PASS (%s)\n' "$name" "$got"; pass=$((pass+1))
  else
    printf '  %-46s FAIL (want %s, got %s)\n' "$name" "$want" "$got"; fail=$((fail+1))
  fi
}

jqp() { python3 -c "import json,sys;d=json.load(sys.stdin);print($1)"; }

# create_job returns the raw POST /jobs response.
create_job() {
  curl -s -X POST "$API/jobs" -H 'Content-Type: application/json' \
    -d "{\"filename\":\"$1\",\"size_bytes\":$FIXTURE_SIZE,\"content_type\":\"video/mp4\"}"
}

# make_fixture writes an 8-second clip well over 5 MiB. The bitrate is forced
# high on purpose: at a sane bitrate an 8-second clip is under 5 MiB and the
# whole thing collapses to one part, which exercises nothing.
#
# `-f mp4` is not optional. ffmpeg picks a muxer from the output file's
# extension, and this fixture is deliberately named .bin — to the upload path it
# is just bytes, and nothing here ever decodes it. Without the flag ffmpeg gives
# up with "Unable to choose an output format for clip.bin", writes nothing, and
# the script then measures a zero-byte fixture and exits claiming the bitrate is
# wrong. That diagnostic sends you to the one line that is not the problem.
make_fixture() {
  ffmpeg -y -loglevel error \
    -f lavfi -i testsrc2=size=1920x1080:rate=30:duration=8 \
    -f lavfi -i sine=frequency=440:duration=8 \
    -c:v libx264 -preset ultrafast -b:v 12M -pix_fmt yuv420p \
    -c:a aac -b:a 128k -t 8 -f mp4 "$work/clip.bin"
}

# put_part uploads a local part file through a presigned URL. See the
# content-type note in the header.
put_part() {
  curl -s -o /dev/null -w '%{http_code}' -X PUT \
    -H 'Content-Type: application/octet-stream' \
    --data-binary @"$2" "$1"
}

# put_part_direct uploads without going through a presigned URL at all, using
# ambient credentials. Used to simulate parts that landed in a previous, now-dead
# process — the whole premise of the resume path is that the server can find
# parts this client never told it about.
put_part_direct() {
  aws s3api upload-part --bucket "$BUCKET" \
    --key "$1" --upload-id "$2" --part-number "$3" --body "$4" >/dev/null
}

# count_mpu returns how many in-flight uploads exist for a key.
#
# Counted in python rather than with --query 'length(Uploads[?...])', which is a
# trap: when nothing is in flight S3 omits Uploads entirely and jmespath's
# length() fails on the null with exit 255. The empty result is the one this is
# asked for most — "is it gone yet" — so the check would break precisely when it
# was supposed to say yes.
count_mpu() {
  aws s3api list-multipart-uploads --bucket "$BUCKET" --output json \
    | python3 -c 'import json,sys
key = sys.argv[1]
print(sum(1 for u in (json.load(sys.stdin).get("Uploads") or []) if u["Key"] == key))' "$1"
}

echo "stage 8A resume verification — api=$API"

make_fixture
FIXTURE_SIZE="$(wc -c < "$work/clip.bin" | tr -d ' ')"
EXPECT_PARTS=$(( (FIXTURE_SIZE + PART_SIZE - 1) / PART_SIZE ))
echo "  fixture: $FIXTURE_SIZE bytes -> $EXPECT_PARTS parts at ${PART_SIZE}B"
# Exactly three, because the checks below name part_aa/ab/ac directly and treat
# part 2 as the one gap. A four-part fixture would leave parts 4+ missing too
# and the "only the missing part" assertion would be measuring the wrong thing.
if [ "$EXPECT_PARTS" -ne 3 ]; then
  echo "  fixture is $EXPECT_PARTS parts; this script assumes 3. Adjust the bitrate." >&2
  exit 1
fi

split -b "$PART_SIZE" "$work/clip.bin" "$work/part_"

# ── The upload plan ───────────────────────────────────────────────────────────
job_json="$(create_job clip.mp4)"
JOB="$(jqp 'd["job_id"]' <<<"$job_json")"
UPLOAD="$(jqp 'd["upload_id"]' <<<"$job_json")"
KEY="$JOB/clip.mp4"

check "POST /jobs returns $EXPECT_PARTS parts" "$EXPECT_PARTS" "$(jqp 'len(d["upload_urls"])' <<<"$job_json")"
check "presigned against the public endpoint" "$EXPECTED_HOST" \
  "$(jqp '__import__("urllib.parse").parse.urlparse(d["upload_urls"][0]["url"]).netloc' <<<"$job_json")"

# ── The kill: parts land out of order, then the client disappears ─────────────
# Out of order on purpose. A killed client's parts are a SET, not a prefix, and
# a resume that assumes a contiguous prefix passes this only by luck.
put_part_direct "$KEY" "$UPLOAD" 3 "$work/part_ac"
put_part_direct "$KEY" "$UPLOAD" 1 "$work/part_aa"

# ── The invisibility demonstration ────────────────────────────────────────────
# Two listings of the same bucket disagree about whether ~7.7 MiB exists. This
# is why DECIDE 4 is a correctness requirement and not a cost one.
in_objects="$(aws s3 ls "s3://$BUCKET/" --recursive | grep -c "$JOB")"
check "abandoned parts absent from s3 ls" 0 "$in_objects"
check "abandoned parts present in list-mpu" 1 "$(count_mpu "$KEY")"

# ── The endpoint the stage exists for ─────────────────────────────────────────
resume="$(curl -s -X POST "$API/jobs/$JOB/upload-urls")"
check "resume lists the 2 parts S3 holds" "1,3" \
  "$(jqp '",".join(str(p["part_number"]) for p in sorted(d["uploaded_parts"], key=lambda p: p["part_number"]))' <<<"$resume")"
check "resume offers ONLY the missing part" "2" \
  "$(jqp '",".join(str(u["part_number"]) for u in d["upload_urls"])' <<<"$resume")"
check "re-issued url keeps the public host" "$EXPECTED_HOST" \
  "$(jqp '__import__("urllib.parse").parse.urlparse(d["upload_urls"][0]["url"]).netloc' <<<"$resume")"
# The ETags must be S3's own. A client that never recorded one still gets them.
check "etags are S3's, quoted as S3 returns them" "True" \
  "$(jqp 'all(p["etag"].startswith(chr(34)) for p in d["uploaded_parts"])' <<<"$resume")"

resume_url="$(jqp 'd["upload_urls"][0]["url"]' <<<"$resume")"

# A re-issued URL must be a real signature, not merely a well-formed string.
# Under the emulator these rows only meant anything because compose set
# S3_SKIP_SIGNATURE_VALIDATION=0; with it on they all passed trivially. Real S3
# has no such switch, so they now measure what they always claimed to.
sig="$(python3 -c "import urllib.parse as u,sys;print(u.parse_qs(u.urlparse(sys.argv[1]).query)['X-Amz-Signature'][0])" "$resume_url")"
bad="$([ "${sig:0:1}" = "a" ] && echo "b${sig:1}" || echo "a${sig:1}")"
check "re-issued url rejects a corrupt signature" 403 "$(put_part "${resume_url/$sig/$bad}" "$work/part_ab")"

# The host swap has to land on a host that actually answers, or the row measures
# DNS instead of the signature. S3's dualstack endpoint is the same bucket in the
# same region under a different name, which is exactly the property needed. The
# old swap was 10.0.2.2:4566 -> localhost:4566, strings that do not occur in a
# real presigned URL: the "tampered" URL was the original, so the row asserted
# that a VALID signature is rejected and could only pass by accident.
rewritten="$(python3 -c '
import re, sys, urllib.parse as u
p = u.urlparse(sys.argv[1])
m = re.match(r"^(.*?)s3[.-]([a-z0-9-]+)\.amazonaws\.com$", p.netloc)
print(u.urlunparse(p._replace(netloc="%ss3.dualstack.%s.amazonaws.com" % (m.group(1), m.group(2)))) if m else "")' "$resume_url")"
if [ -z "$rewritten" ]; then
  printf '  %-46s SKIP (not an S3 endpoint this can rewrite)\n' "re-issued url rejects a rewritten host"
else
  check "re-issued url rejects a rewritten host" 403 \
    "$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/octet-stream' \
       --data-binary @"$work/part_ab" "$rewritten")"
fi
check "re-issued url rejects a tampered part no." 403 \
  "$(put_part "${resume_url/partNumber=2/partNumber=3}" "$work/part_ab")"
check "re-issued url accepts the real PUT" 200 "$(put_part "$resume_url" "$work/part_ab")"

check "resume is idempotent: nothing left missing" 0 \
  "$(curl -s -X POST "$API/jobs/$JOB/upload-urls" | jqp 'len(d["upload_urls"])')"

# ── Completion with no client-held state whatsoever ───────────────────────────
# No body at all. The server derives the part list, in order, with matching
# ETags, from ListParts. This is what makes "the client never persists an ETag"
# true rather than nearly true.
check "POST /complete with NO body" 200 \
  "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/jobs/$JOB/complete")"
check "the assembled object is byte-exact" "$FIXTURE_SIZE" \
  "$(aws s3api head-object --bucket "$BUCKET" --key "$KEY" \
     --query ContentLength --output text)"

# ── Error codes. The client's recovery differs per code, so a generic 500 or a
#    catch-all 400 would be worse than no code at all. ─────────────────────────
check "unknown job -> 404" 404 \
  "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/jobs/00000000-0000-0000-0000-000000000000/upload-urls")"
check "completed job -> 409 ALREADY_COMPLETE" "409 ALREADY_COMPLETE" \
  "$(curl -s -w ' %{http_code}' -X POST "$API/jobs/$JOB/upload-urls" \
    | python3 -c "import json,sys;t=sys.stdin.read();b,c=t.rsplit(' ',1);print(c,json.loads(b)['code'])")"

# ── Abort, and what the client sees afterwards ────────────────────────────────
job2_json="$(create_job cancelled.mp4)"
JOB2="$(jqp 'd["job_id"]' <<<"$job2_json")"
put_part_direct "$JOB2/cancelled.mp4" "$(jqp 'd["upload_id"]' <<<"$job2_json")" 1 "$work/part_aa"

check "DELETE /jobs/{id}/upload" 200 \
  "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$API/jobs/$JOB2/upload")"
check "the parts are actually gone from S3" 0 "$(count_mpu "$JOB2/cancelled.mp4")"
check "abort twice is not an error" 200 \
  "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$API/jobs/$JOB2/upload")"
# 410, not 500. Without a distinguishable code the client retries forever
# against an upload that no longer exists.
check "resume after abort -> 410 UPLOAD_GONE" "410 UPLOAD_GONE" \
  "$(curl -s -w ' %{http_code}' -X POST "$API/jobs/$JOB2/upload-urls" \
    | python3 -c "import json,sys;t=sys.stdin.read();b,c=t.rsplit(' ',1);print(c,json.loads(b)['code'])")"

# ── Truncation. S3 will assemble an object from a subset of its parts and
#    return 200, so a client that completes early gets a silently short video
#    that fails several stages away as an ffprobe error. ────────────────────────
job3_json="$(create_job partial.mp4)"
JOB3="$(jqp 'd["job_id"]' <<<"$job3_json")"
put_part_direct "$JOB3/partial.mp4" "$(jqp 'd["upload_id"]' <<<"$job3_json")" 1 "$work/part_aa"
check "complete with 1 of 3 parts -> 409" "409 INCOMPLETE_UPLOAD" \
  "$(curl -s -w ' %{http_code}' -X POST "$API/jobs/$JOB3/complete" \
    | python3 -c "import json,sys;t=sys.stdin.read();b,c=t.rsplit(' ',1);print(c,json.loads(b)['code'])")"
check "a stale upload_id -> 409 UPLOAD_MISMATCH" "409 UPLOAD_MISMATCH" \
  "$(curl -s -w ' %{http_code}' -X POST "$API/jobs/$JOB3/complete" -H 'Content-Type: application/json' \
     -d '{"upload_id":"an-upload-id-from-some-other-job"}' \
    | python3 -c "import json,sys;t=sys.stdin.read();b,c=t.rsplit(' ',1);print(c,json.loads(b)['code'])")"
curl -s -o /dev/null -X DELETE "$API/jobs/$JOB3/upload"

# ── The path stage 7's uploader uses, which must not break ────────────────────
# The tolerant completion above is additive. A client that assembles the parts
# array itself still has to work, because one already does.
job4_json="$(create_job supplied.mp4)"
JOB4="$(jqp 'd["job_id"]' <<<"$job4_json")"
UPLOAD4="$(jqp 'd["upload_id"]' <<<"$job4_json")"
i=1
for suffix in aa ab ac; do
  put_part_direct "$JOB4/supplied.mp4" "$UPLOAD4" "$i" ""$work/part_$suffix""
  i=$((i+1))
done
# Built in python, not by shell interpolation: S3's ETags arrive already
# quoted, and pasting them into a hand-written JSON string produces a body the
# server rejects as malformed — which reads exactly like a server bug.
body4="$(aws s3api list-parts --bucket "$BUCKET" \
  --key "$JOB4/supplied.mp4" --upload-id "$UPLOAD4" --output json \
  | python3 -c 'import json,sys
parts = json.load(sys.stdin)["Parts"]
print(json.dumps({"upload_id": sys.argv[1],
                  "parts": [{"part_number": p["PartNumber"], "etag": p["ETag"]} for p in parts]}))' "$UPLOAD4")"
check "complete with a client-supplied parts array" 200 \
  "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/jobs/$JOB4/complete" \
     -H 'Content-Type: application/json' --data-binary "$body4")"

# ── The pipeline must not be able to tell ─────────────────────────────────────
# A resumed upload is just an object. If this ever fails, resume is producing a
# file that differs from a fresh upload's, which every check above would miss.
status=""
for _ in $(seq 1 30); do
  status="$(curl -s "$API/jobs/$JOB" | jqp 'd["status"]')"
  [ "$status" = "completed" ] || [ "$status" = "failed" ] && break
  sleep 5
done
check "the pipeline ran on the resumed upload" "completed" "$status"
check "GET /jobs/{id}/reel" 200 "$(curl -s -o /dev/null -w '%{http_code}' "$API/jobs/$JOB/reel")"

echo
echo "pass=$pass fail=$fail"
echo
echo "NOT PROVEN HERE, and neither is provable in a session:"
echo "  - the lifecycle rule's effect. It has never been observed to work: under"
echo "    the emulator it was accepted, read back verbatim and never acted on."
echo "  - ListParts paging. The loop exists and 3 parts never reach page two."
echo
echo "THIS RUN LEFT BILLABLE STATE. Job $JOB completed, so its raw object and"
echo "its HLS output are still in S3. Run scripts/abort-stale-uploads.sh to"
echo "check for parts still in flight, and delete the objects when finished:"
echo "  aws s3 rm s3://$BUCKET/$JOB/ --recursive"
[ "$fail" -eq 0 ]
