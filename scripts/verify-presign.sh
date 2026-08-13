#!/usr/bin/env bash
#
# Negative-test matrix for presigned upload URLs.
#
# A successful upload proves nothing on its own. If the server accepts
# everything, a correct signature and a garbage one both return 200 — which is
# exactly what LocalStack does out of the box, and exactly how the SigV4
# host-binding bug survived undetected until stage 7. Every check below breaks
# one property of a freshly issued URL and asserts the server rejects it.
#
#   ./scripts/verify-presign.sh              # against the local stack
#   TARGET=aws API=https://... ./scripts/verify-presign.sh
#
# Requires S3_SKIP_SIGNATURE_VALIDATION=0 on LocalStack (set in compose).
# Without it every row below returns 200 and the matrix is meaningless.
set -uo pipefail

API="${API:-http://localhost:8080}"
TARGET="${TARGET:-localstack}"

payload="$(mktemp)"
trap 'rm -f "$payload"' EXIT
printf 'dayreel-presign-negative-test' > "$payload"

pass=0; fail=0; gap=0

status() { curl -s -o /dev/null -w '%{http_code}' -X PUT --data-binary @"$payload" "$1"; }

# new_url issues a fresh job and returns its part-1 upload URL. Each check gets
# its own URL so a consumed or poisoned upload cannot affect the next row.
new_url() {
  curl -s -X POST "$API/jobs" \
    -H 'Content-Type: application/json' \
    -d '{"filename":"neg.mp4","size_bytes":29,"content_type":"video/mp4"}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["upload_urls"][0]["url"])'
}

# check NAME EXPECTED ACTUAL [known-gap-on-localstack]
check() {
  local name="$1" want="$2" got="$3" gap_ok="${4:-}"
  if [ "$got" = "$want" ]; then
    printf '  %-32s %s\n' "$name" "PASS (${got})"
    pass=$((pass+1))
  elif [ -n "$gap_ok" ] && [ "$TARGET" = "localstack" ]; then
    printf '  %-32s GAP  (want %s, got %s) %s\n' "$name" "$want" "$got" "$gap_ok"
    gap=$((gap+1))
  else
    printf '  %-32s FAIL (want %s, got %s)\n' "$name" "$want" "$got"
    fail=$((fail+1))
  fi
}

# rewrite applies a python transform to a URL's parsed form.
rewrite() { python3 -c "$1" "$2"; }

echo "presigned URL negative matrix — target=$TARGET api=$API"

# A valid URL must work, or every rejection below is trivially explained by the
# upload being broken rather than by the tampering being detected.
check "valid signature" 200 "$(status "$(new_url)")"

# The signature value itself must be checked, not merely present. Only one hex
# character changes, so nothing about the request's shape differs.
u="$(new_url)"
sig="$(rewrite 'import sys,urllib.parse as u;print(u.parse_qs(u.urlparse(sys.argv[1]).query)["X-Amz-Signature"][0])' "$u")"
bad="$( [ "${sig:0:1}" = "a" ] && echo "b${sig:1}" || echo "a${sig:1}" )"
check "corrupted signature" 403 "$(status "${u/$sig/$bad}")"

# The host is part of the signed canonical request. This is the property
# S3_PUBLIC_ENDPOINT exists to preserve: a URL signed for an in-cluster hostname
# cannot simply be rewritten to one the client can reach.
u="$(new_url)"
check "rewritten host" 403 "$(status "${u/localhost:4566/127.0.0.1:4566}")"

# The object key is signed, so a URL for one key must not upload to another.
u="$(new_url)"
check "tampered object key" 403 \
  "$(status "$(rewrite 'import sys,urllib.parse as u;p=u.urlparse(sys.argv[1]);print(u.urlunparse(p._replace(path=p.path+"x")))' "$u")")"

# Signed query parameters must be immutable too, not just the path.
u="$(new_url)"
check "tampered partNumber" 403 "$(status "${u/partNumber=1/partNumber=2}")"

# Shortening the expiry window changes a signed parameter; a client must not be
# able to lengthen it either.
u="$(new_url)"
check "tampered expiry window" 403 \
  "$(status "$(rewrite 'import sys,urllib.parse as u;p=u.urlparse(sys.argv[1]);q=[(k,"1" if k=="X-Amz-Expires" else v) for k,v in u.parse_qsl(p.query)];print(u.urlunparse(p._replace(query=u.urlencode(q))))' "$u")")"

# The diagnostic row. If an unsigned request succeeds then the bucket accepts
# anonymous writes, and holding a presigned URL confers no privilege at all.
#
# LocalStack Community does not enforce bucket authorization, so this is a
# known local gap rather than a defect in this repo. Real S3 has Block Public
# Access enabled by default and denies this — which is an assumption that must
# be ASSERTED when real buckets are provisioned, not trusted.
u="$(new_url)"
check "anonymous PUT (no signature)" 403 \
  "$(status "$(rewrite 'import sys,urllib.parse as u;p=u.urlparse(sys.argv[1]);q=[(k,v) for k,v in u.parse_qsl(p.query) if not k.startswith("X-Amz-")];print(u.urlunparse(p._replace(query=u.urlencode(q))))' "$u")")" \
  "<- LocalStack does not enforce bucket auth; must hold on real S3"

echo "pass=$pass fail=$fail gap=$gap"
[ "$fail" -eq 0 ]
