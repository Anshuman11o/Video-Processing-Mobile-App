#!/usr/bin/env bash
#
# Negative-test matrix for presigned upload URLs, against real AWS S3.
#
# A successful upload proves nothing on its own. If the server accepts
# everything, a correct signature and a garbage one both return 200 — which is
# exactly what LocalStack did out of the box, and exactly how the SigV4
# host-binding bug survived undetected until stage 7. Every check below breaks
# one property of a freshly issued URL and asserts the server rejects it.
#
#   ./scripts/verify-presign.sh
#   API=https://... ./scripts/verify-presign.sh
#
# THERE IS NO `TARGET` ANY MORE. It chose between LocalStack and real AWS, and
# LocalStack is gone: S3 is a real account, and real S3 validates presigned
# signatures unconditionally, so there is nothing to configure to make these
# rows mean anything. The anonymous row in particular is now a HARD FAILURE
# rather than the tolerated local gap it used to be — asserting it against real
# buckets is the whole point of running this.
#
# EVERY ROW MINTS A REAL MULTIPART UPLOAD. Parts of an unfinished upload bill as
# storage and appear in no object listing, so this aborts every job it created
# on exit (DELETE /jobs/{id}/upload, which is idempotent). If it is killed before
# the trap runs, ./scripts/abort-stale-uploads.sh --abort releases them.
#
# UNVERIFIED: this has not been run against a real AWS account. The expected
# codes below are what SigV4 and Block Public Access specify, not what was
# observed. What HAS been exercised is the script's own mechanics — job
# creation, each transform, the cleanup — against a stand-in server on
# localhost, which proves nothing about S3. See docs/SETUP.md.
set -uo pipefail

API="${API:-http://localhost:8080}"

payload="$(mktemp)"
printf 'dayreel-presign-negative-test' > "$payload"

# Space separated rather than an array: every job id is a UUID, and this stays
# legible under `set -u` without the empty-array incantation.
created_jobs=""

cleanup() {
  rm -f "$payload"
  for job in $created_jobs; do
    curl -s -o /dev/null -X DELETE "$API/jobs/$job/upload"
  done
}
trap cleanup EXIT

pass=0; fail=0; skip=0

status() { curl -s -o /dev/null -w '%{http_code}' -X PUT --data-binary @"$payload" "$1"; }

# new_url issues a fresh job and sets URL to its part-1 upload URL. Each check
# gets its own URL so a consumed or poisoned upload cannot affect the next row.
#
# It sets globals instead of printing, because the job id has to be recorded for
# the cleanup trap and an assignment inside "$(new_url)" would happen in a
# subshell and be lost.
URL=""
new_url() {
  local body
  body="$(curl -s -X POST "$API/jobs" \
    -H 'Content-Type: application/json' \
    -d '{"filename":"neg.mp4","size_bytes":29,"content_type":"video/mp4"}')"
  created_jobs="$created_jobs $(printf '%s' "$body" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["job_id"])')"
  URL="$(printf '%s' "$body" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["upload_urls"][0]["url"])')"
}

# check NAME EXPECTED ACTUAL
check() {
  local name="$1" want="$2" got="$3"
  if [ "$got" = "$want" ]; then
    printf '  %-32s %s\n' "$name" "PASS (${got})"
    pass=$((pass+1))
  else
    printf '  %-32s FAIL (want %s, got %s)\n' "$name" "$want" "$got"
    fail=$((fail+1))
  fi
}

# rewrite applies a python transform to a URL's parsed form.
rewrite() { python3 -c "$1" "$2"; }

echo "presigned URL negative matrix — api=$API"

# A valid URL must work, or every rejection below is trivially explained by the
# upload being broken rather than by the tampering being detected.
new_url
check "valid signature" 200 "$(status "$URL")"

# The signature value itself must be checked, not merely present. Only one hex
# character changes, so nothing about the request's shape differs.
new_url; u="$URL"
sig="$(rewrite 'import sys,urllib.parse as u;print(u.parse_qs(u.urlparse(sys.argv[1]).query)["X-Amz-Signature"][0])' "$u")"
bad="$( [ "${sig:0:1}" = "a" ] && echo "b${sig:1}" || echo "a${sig:1}" )"
check "corrupted signature" 403 "$(status "${u/$sig/$bad}")"

# The host is part of the signed canonical request, so a URL signed for one
# hostname cannot be replayed against another. This is the property
# S3_PUBLIC_ENDPOINT exists to preserve, and the one that cost stage 7.
#
# The rewrite has to reach a host that still answers, or the row would be
# measuring DNS instead of the signature. S3's dualstack endpoint is the same
# bucket in the same region under a different name, which is exactly the
# distinction under test. A URL that is not an amazonaws.com S3 endpoint —
# S3_PUBLIC_ENDPOINT pointed at a proxy — cannot be rewritten this way, and the
# row says so rather than passing vacuously on an unmodified URL.
new_url; u="$URL"
alt="$(rewrite 'import re,sys,urllib.parse as u
p = u.urlparse(sys.argv[1])
# Only the REGIONAL form can be rewritten this way. s3.dualstack.<region>...
# is a real endpoint; s3.dualstack.amazonaws.com is not, so a legacy global
# host must fall through to the skip rather than be turned into a DNS failure
# that would read as a signature rejection.
m = re.match(r"^((?:.+\.)?)s3\.([a-z0-9-]+)\.amazonaws\.com$", p.netloc)
print(u.urlunparse(p._replace(
    netloc="%ss3.dualstack.%s.amazonaws.com" % (m.group(1), m.group(2)))) if m else "")' "$u")"
if [ -z "$alt" ]; then
  printf '  %-32s SKIP (not an S3 endpoint this can rewrite)\n' "rewritten host"
  skip=$((skip+1))
else
  check "rewritten host" 403 "$(status "$alt")"
fi

# The object key is signed, so a URL for one key must not upload to another.
new_url; u="$URL"
check "tampered object key" 403 \
  "$(status "$(rewrite 'import sys,urllib.parse as u;p=u.urlparse(sys.argv[1]);print(u.urlunparse(p._replace(path=p.path+"x")))' "$u")")"

# Signed query parameters must be immutable too, not just the path.
new_url; u="$URL"
check "tampered partNumber" 403 "$(status "${u/partNumber=1/partNumber=2}")"

# Shortening the expiry window changes a signed parameter; a client must not be
# able to lengthen it either.
new_url; u="$URL"
check "tampered expiry window" 403 \
  "$(status "$(rewrite 'import sys,urllib.parse as u;p=u.urlparse(sys.argv[1]);q=[(k,"1" if k=="X-Amz-Expires" else v) for k,v in u.parse_qsl(p.query)];print(u.urlunparse(p._replace(query=u.urlencode(q))))' "$u")")"

# The diagnostic row, and the reason this script is worth running against real
# buckets at all. If an unsigned request succeeds then the bucket accepts
# anonymous writes and holding a presigned URL confers no privilege whatsoever.
#
# This was a known gap under LocalStack, which did not enforce bucket
# authorization. Real S3 has Block Public Access on by default and must deny it.
# That is the assumption the entire upload design rests on, so it is asserted
# here rather than trusted.
new_url; u="$URL"
check "anonymous PUT (no signature)" 403 \
  "$(status "$(rewrite 'import sys,urllib.parse as u;p=u.urlparse(sys.argv[1]);q=[(k,v) for k,v in u.parse_qsl(p.query) if not k.startswith("X-Amz-")];print(u.urlunparse(p._replace(query=u.urlencode(q))))' "$u")")"

echo "pass=$pass fail=$fail skip=$skip"
[ "$fail" -eq 0 ]
