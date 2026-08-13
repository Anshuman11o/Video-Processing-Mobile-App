#!/usr/bin/env bash
#
# Turns public-read on and off for the HLS output bucket, on REAL AWS.
#
# WHY THIS EXISTS AT ALL
#
# HLS playlists reference their segments by relative path. A presigned master
# playlist is followed by a 403 on every segment, because the segment URIs carry
# no signature and nothing in the format lets a player add one. So presigning
# cannot deliver HLS without rewriting every URI in every variant playlist.
#
# Locally this has never surfaced, and that is the trap: LocalStack Community
# serves unsigned GETs to any bucket, so playback "works" for a reason that does
# not hold on real S3. See "LocalStack does not enforce bucket authorization" in
# docs/SETUP.md.
#
# On real AWS the reel still has to be reachable. This script is the public-read
# option, deliberately opt-in and off by default. docs/aws-public-hls.md has the
# full argument, including the alternative that was NOT taken.
#
#   ./scripts/aws-hls-public.sh status
#   ./scripts/aws-hls-public.sh status --probe-key <job-uuid>/master.m3u8
#   ./scripts/aws-hls-public.sh enable  --yes
#   ./scripts/aws-hls-public.sh disable --yes
#
# THE BUCKET NAME IS NOT A CONSTANT. S3 bucket names are globally unique, so the
# name in .env is the only one this project owns; a literal in here is a name
# belonging to somebody else. Export the environment first — `set -a; . ./.env;
# set +a` — the way the Makefile does for the Go processes. BUCKET= and REGION=
# still override.
#
# WHAT HAS ACTUALLY BEEN RUN
#
# Exercised against real S3 on 2026-08-13, account 384627056323. With bucket BPA
# on and no policy, an anonymous GET of master.m3u8 was 403. After `enable`, the
# master playlist, all three variant playlists, the .ts segments, the subtitle
# playlist, the .vtt and thumbnail.jpg all returned 200 anonymously — while an
# anonymous bucket LIST still returned 403, which is the GetObject-without-
# ListBucket split below doing exactly what it claims. `disable` put it back.
# Account-level BPA is not configured on that account, so step 1 was skipped and
# --allow-account-bpa-change was never needed.
#
# What remains untestable locally is unchanged: LocalStack accepts
# put-bucket-policy and put-public-access-block, reads both back verbatim, and
# enforces neither. A run there would print an unbroken column of OK and prove
# nothing, which is why the guard below refuses it.
set -uo pipefail

# Same resolution order as scripts/verify-resume.sh: an explicit override, then
# the name the rest of the stack uses, then a last-resort literal that exists so
# `--help` still prints something. Like every other script here this does NOT
# source .env itself — the caller exports it, or make does.
BUCKET="${BUCKET:-${S3_HLS_BUCKET:-dayreel-hls-output}}"
REGION="${REGION:-${AWS_REGION:-$(aws configure get region 2>/dev/null || echo us-east-1)}}"

# Where `enable` records the settings it displaced, so `disable` can put them
# back rather than guess. Kept out of the repo on purpose — it names a real
# account. `enable` also prints it to stdout, so losing the file is recoverable.
STATE_DIR="${DAYREEL_STATE_DIR:-${TMPDIR:-/tmp}}"
STATE_FILE="$STATE_DIR/dayreel-hls-public.$BUCKET.json"

# The four Block Public Access settings, as `enable` wants them at bucket level.
#
# Only the two POLICY settings are relaxed. BlockPublicAcls and IgnorePublicAcls
# stay on because access here is granted by bucket policy, not by ACL — leaving
# the ACL guards up costs nothing and keeps a stray `--acl public-read` on some
# future upload from becoming a second, unmanaged way in.
BPA_OPEN="BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=false,RestrictPublicBuckets=false"
BPA_SHUT="BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true"

# The policy. Every omission in it is load-bearing; see the block comment on
# bucket_policy() below.
bucket_policy() {
  cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "PublicReadHLSObjects",
      "Effect": "Allow",
      "Principal": "*",
      "Action": "s3:GetObject",
      "Resource": "arn:aws:s3:::$BUCKET/*"
    }
  ]
}
EOF
}
#
# WHY s3:GetObject ON /* AND DELIBERATELY NOT s3:ListBucket
#
# GetObject on "arn:aws:s3:::<bucket>/*" grants reads to callers who already
# know the full key. Keys here are "<job_id>/master.m3u8" and
# "<job_id>/<rendition>/segment_NNN.ts" (see OutputKey and the upload loop in
# backend/internal/worker/packager/packager.go), and job_id is a UUID. An
# anonymous caller therefore needs a 122-bit secret to read one reel, and
# guessing gets them nothing.
#
# s3:ListBucket is a different grant on a different resource — the bare bucket
# ARN, "arn:aws:s3:::<bucket>", with no "/*". Adding it would let anyone on the
# internet call ListObjectsV2 and enumerate every job the pipeline has ever
# processed, turning the unguessable key into a published index. The secrecy of
# the key is the ONLY thing standing between "reachable by the person who
# uploaded it" and "reachable by everyone", so ListBucket must never appear
# here. It is not an oversight and it is not a hardening TODO.

# ── argument parsing ──────────────────────────────────────────────────────────
CMD="${1:-}"; shift 2>/dev/null
CONFIRMED=""
ALLOW_ACCOUNT=""
PROBE_KEY=""

while [ $# -gt 0 ]; do
  case "$1" in
    --yes) CONFIRMED=1 ;;
    --allow-account-bpa-change) ALLOW_ACCOUNT=1 ;;
    --probe-key) PROBE_KEY="${2:-}"; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done

# usage reprints the header comment. Line 46 is the last line of it — keep this
# in step with the header, or the banner ends mid-sentence.
usage() {
  sed -n '2,46p' "$0" | sed 's/^#\{1,2\} \{0,1\}//'
  exit 2
}

case "$CMD" in
  status|enable|disable) ;;
  *) usage ;;
esac

# jsonq runs a python expression over stdin-as-JSON. Same pattern as
# verify-resume.sh — python3 rather than jq, which is not a dependency here.
jsonq() { python3 -c "import json,sys;d=json.load(sys.stdin);print($1)" 2>/dev/null; }

# ── the LocalStack guard ──────────────────────────────────────────────────────
#
# This exists because the failure it prevents is invisible. LocalStack accepts
# every call below, returns success, and reads the configuration back
# unchanged — while enforcing none of it. A run against LocalStack would print
# an unbroken column of OK and prove precisely nothing, and this project has
# already lost two cycles to exactly that shape of false confidence.
if [ -n "${AWS_ENDPOINT_URL:-}${AWS_ENDPOINT_URL_S3:-}" ]; then
  echo "refusing to run: AWS_ENDPOINT_URL is set." >&2
  echo "This script is meaningless against LocalStack, which stores bucket" >&2
  echo "policies and public-access-block settings and then ignores them." >&2
  exit 1
fi

echo "dayreel HLS public access — $CMD"
echo "  bucket: $BUCKET   region: $REGION"

ACCOUNT="$(aws sts get-caller-identity --query Account --output text 2>/dev/null)"
if [ -z "$ACCOUNT" ] || [ "$ACCOUNT" = "None" ]; then
  echo "  cannot resolve the caller's account. Check credentials." >&2
  exit 1
fi
if [ "$ACCOUNT" = "000000000000" ]; then
  echo "  account is 000000000000 — that is LocalStack's. Refusing." >&2
  exit 1
fi
echo "  account: $ACCOUNT"

if ! aws s3api head-bucket --bucket "$BUCKET" --region "$REGION" >/dev/null 2>&1; then
  echo "  bucket $BUCKET is not reachable (missing, or not yours). Refusing." >&2
  exit 1
fi

# ── reading the current state ─────────────────────────────────────────────────
#
# Read before deciding anything. Both `enable` and `disable` are supposed to be
# safe to re-run, and that is only true if they can tell what is already done.

# account_bpa echoes the four account-level settings as a python dict literal,
# or the string NONE.
#
# NONE is the ORDINARY case, not an error: account-level Block Public Access is
# not configured by default. GetPublicAccessBlock answers 404
# NoSuchPublicAccessBlockConfiguration on an account that has never set it, and
# nothing about creating a bucket sets it. The April 2023 "on by default" change
# applies to new BUCKETS. Treating NONE as a failure would send an operator off
# to disable an account-wide guardrail that was never on.
account_bpa() {
  local out
  out="$(aws s3control get-public-access-block --account-id "$ACCOUNT" --region "$REGION" --output json 2>/dev/null)"
  if [ -z "$out" ]; then echo "NONE"; return; fi
  echo "$out" | jsonq 'json.dumps(d["PublicAccessBlockConfiguration"])'
}

bucket_bpa() {
  local out
  out="$(aws s3api get-public-access-block --bucket "$BUCKET" --region "$REGION" --output json 2>/dev/null)"
  if [ -z "$out" ]; then echo "NONE"; return; fi
  echo "$out" | jsonq 'json.dumps(d["PublicAccessBlockConfiguration"])'
}

# has_public_policy is yes when our Sid is present. Matched on the Sid rather
# than on a whole-document comparison so that a policy someone else extended
# still reads as "ours is in there".
#
# The ||-fallback is not decoration. A bucket with no policy answers
# NoSuchBucketPolicy, jsonq gets empty stdin and prints nothing, and an empty
# string here would render as a blank field that reads like a bug in this
# script rather than as the ordinary "there is no policy" case.
has_public_policy() {
  local out
  out="$(aws s3api get-bucket-policy --bucket "$BUCKET" --region "$REGION" --output json 2>/dev/null \
    | jsonq '"yes" if "PublicReadHLSObjects" in d.get("Policy","") else "no"')"
  echo "${out:-no}"
}

# flag_of pulls one setting out of a BPA JSON blob. Absent config reads as
# false, which is what S3 does with it.
flag_of() {
  [ "$1" = "NONE" ] && { echo "false"; return; }
  echo "$1" | jsonq "str(d.get(\"$2\", False)).lower()"
}

ACCT_JSON="$(account_bpa)"
BKT_JSON="$(bucket_bpa)"
POLICY_PRESENT="$(has_public_policy)"

echo
echo "current state"
echo "  account BPA : $ACCT_JSON"
echo "  bucket  BPA : $BKT_JSON"
echo "  our policy  : $POLICY_PRESENT"

# is_public is S3's own verdict, not ours. Worth printing because it accounts
# for the interaction of policy and BPA in a way that reading either alone does
# not — S3 applies the MOST RESTRICTIVE combination of account and bucket
# settings, so a permissive bucket under a locked-down account is still shut.
POLICY_STATUS="$(aws s3api get-bucket-policy-status --bucket "$BUCKET" --region "$REGION" --output json 2>/dev/null | jsonq 'd["PolicyStatus"]["IsPublic"]')"
echo "  S3 says public: ${POLICY_STATUS:-<no policy>}"

ACCT_BLOCKS_POLICY="false"
if [ "$(flag_of "$ACCT_JSON" BlockPublicPolicy)" = "true" ] || \
   [ "$(flag_of "$ACCT_JSON" RestrictPublicBuckets)" = "true" ]; then
  ACCT_BLOCKS_POLICY="true"
fi

# ── the anonymous probe ───────────────────────────────────────────────────────
#
# The only check that measures the thing anyone actually cares about. Everything
# above reads configuration back from the API, which is exactly the class of
# evidence that has been wrong here before. curl with no credentials is what a
# stranger's player is.
probe() {
  [ -z "$PROBE_KEY" ] && return
  local url="https://$BUCKET.s3.$REGION.amazonaws.com/$PROBE_KEY"
  echo
  echo "anonymous GET $url"
  echo "  HTTP $(curl -s -o /dev/null -w '%{http_code}' "$url")   (200 = readable by anyone; 403 = not)"
}

if [ "$CMD" = "status" ]; then
  probe
  exit 0
fi

# ── confirmation ──────────────────────────────────────────────────────────────
#
# Printed before the flag is checked, so that a run without --yes is a dry run
# that shows the exact calls rather than an error that shows nothing.
echo
echo "this $CMD would run:"
if [ "$CMD" = "enable" ]; then
  if [ "$ACCT_BLOCKS_POLICY" = "true" ]; then
    echo "  aws s3control put-public-access-block --account-id $ACCOUNT \\"
    echo "    --public-access-block-configuration $BPA_OPEN"
    echo "    ^ ACCOUNT-WIDE. This removes the public-policy guardrail from EVERY"
    echo "      bucket in account $ACCOUNT, not just $BUCKET, for as long as it is off."
  else
    echo "  (account-level BPA does not block public policies; nothing to change there)"
  fi
  echo "  aws s3api put-public-access-block --bucket $BUCKET \\"
  echo "    --public-access-block-configuration $BPA_OPEN"
  echo "  aws s3api put-bucket-policy --bucket $BUCKET --policy <<"
  bucket_policy | sed 's/^/    /'
else
  echo "  aws s3api delete-bucket-policy --bucket $BUCKET"
  echo "  aws s3api put-public-access-block --bucket $BUCKET \\"
  echo "    --public-access-block-configuration $BPA_SHUT"
  echo "  aws s3control put-public-access-block --account-id $ACCOUNT   (restore, if enable changed it)"
fi

if [ -z "$CONFIRMED" ]; then
  echo
  echo "DRY RUN. Nothing was changed. Re-run with --yes to apply."
  exit 0
fi

# ── enable ────────────────────────────────────────────────────────────────────
if [ "$CMD" = "enable" ]; then
  echo
  # Order matters. BlockPublicPolicy=true makes PutBucketPolicy reject a public
  # policy outright, so the guard has to come down before the policy goes up.
  # Doing it the other way round fails with AccessDenied and reads like a
  # credentials problem.

  if [ "$ACCT_BLOCKS_POLICY" = "true" ]; then
    # The account-level change is gated behind a SECOND flag on purpose. Its
    # blast radius is every bucket in the account, which is categorically
    # different from the bucket-level change and should not ride along on the
    # same --yes.
    if [ -z "$ALLOW_ACCOUNT" ]; then
      echo "STOP. Account-level Block Public Access blocks public bucket policies," >&2
      echo "and S3 applies the most restrictive of the account and bucket settings," >&2
      echo "so relaxing $BUCKET alone will NOT make it readable." >&2
      echo >&2
      echo "Changing it affects every bucket in account $ACCOUNT. If that is" >&2
      echo "genuinely what you want, re-run with --allow-account-bpa-change." >&2
      echo "Read docs/aws-public-hls.md first." >&2
      exit 1
    fi
    # Preserve the ACL flags as found rather than forcing them; the operator may
    # have set them deliberately and this script has no business with ACLs.
    keep_acls="BlockPublicAcls=$(flag_of "$ACCT_JSON" BlockPublicAcls),IgnorePublicAcls=$(flag_of "$ACCT_JSON" IgnorePublicAcls)"
    aws s3control put-public-access-block --account-id "$ACCOUNT" --region "$REGION" \
      --public-access-block-configuration "$keep_acls,BlockPublicPolicy=false,RestrictPublicBuckets=false" \
      && echo "  OK  account BPA relaxed (policy settings only)" \
      || { echo "  FAIL account BPA — an AWS Organizations BPA policy overrides the account and cannot be changed from here" >&2; exit 1; }
  fi

  aws s3api put-public-access-block --bucket "$BUCKET" --region "$REGION" \
    --public-access-block-configuration "$BPA_OPEN" \
    && echo "  OK  bucket BPA relaxed (policy settings only; ACL guards left on)" \
    || { echo "  FAIL bucket BPA" >&2; exit 1; }

  aws s3api put-bucket-policy --bucket "$BUCKET" --region "$REGION" \
    --policy "$(bucket_policy)" \
    && echo "  OK  public-read policy applied" \
    || { echo "  FAIL put-bucket-policy" >&2; exit 1; }

  # Record what was displaced. Re-running enable must not overwrite a good
  # record with the already-open state it just created, or disable would
  # "restore" the bucket to public.
  if [ ! -f "$STATE_FILE" ]; then
    printf '{"account":"%s","account_bpa_before":%s,"bucket_bpa_before":%s}\n' \
      "$ACCOUNT" \
      "$( [ "$ACCT_JSON" = "NONE" ] && echo null || echo "$ACCT_JSON" )" \
      "$( [ "$BKT_JSON" = "NONE" ] && echo null || echo "$BKT_JSON" )" \
      > "$STATE_FILE"
  fi

  probe
  echo
  echo "=============================================================="
  echo " $BUCKET IS NOW WORLD-READABLE. TURN IT BACK OFF:"
  echo "   ./scripts/aws-hls-public.sh disable --yes"
  echo
  echo " Every GET and every byte out is billed to account $ACCOUNT."
  echo " The project budget is \$20 total — see config/free-tier.md."
  if [ "$ACCT_BLOCKS_POLICY" = "true" ]; then
    echo
    echo " ACCOUNT-LEVEL Block Public Access is now OFF for account $ACCOUNT."
    echo " EVERY bucket in this account has lost that guardrail, not just this one."
  fi
  echo "=============================================================="
  echo
  echo "state recorded for disable (keep this if $STATE_FILE is lost):"
  cat "$STATE_FILE"
  exit 0
fi

# ── disable ───────────────────────────────────────────────────────────────────
#
# Reverses all of it. Policy first: after that call the bucket is private
# regardless of what any BPA restore does or fails to do, so a partial run
# leaves the safe state rather than the exposed one.
echo
restored=""

aws s3api delete-bucket-policy --bucket "$BUCKET" --region "$REGION" 2>/dev/null \
  && { echo "  OK  bucket policy deleted"; restored="$restored policy"; } \
  || echo "  --  no bucket policy to delete"

aws s3api put-public-access-block --bucket "$BUCKET" --region "$REGION" \
  --public-access-block-configuration "$BPA_SHUT" \
  && { echo "  OK  bucket BPA: all four settings ON"; restored="$restored bucket-bpa"; } \
  || echo "  FAIL bucket BPA — the bucket may still accept a public policy" >&2

# Account level is only touched if this script turned it off, and only back to
# what it found. Guessing here would be worse than doing nothing: an account
# that deliberately ran a partial configuration should not silently acquire a
# full one because a video project borrowed its bucket for an afternoon.
if [ -f "$STATE_FILE" ]; then
  before="$(jsonq 'json.dumps(d["account_bpa_before"])' < "$STATE_FILE")"
  if [ -n "$before" ] && [ "$before" != "null" ]; then
    cfg="$(echo "$before" | jsonq '",".join(f"{k}={str(v).lower()}" for k,v in d.items())')"
    aws s3control put-public-access-block --account-id "$ACCOUNT" --region "$REGION" \
      --public-access-block-configuration "$cfg" \
      && { echo "  OK  account BPA restored to: $cfg"; restored="$restored account-bpa"; } \
      || echo "  FAIL account BPA restore — set it by hand, it is account-wide" >&2
  else
    echo "  --  account BPA was unset before enable; left unset"
  fi
  rm -f "$STATE_FILE"
else
  echo "  --  no state file at $STATE_FILE"
  if [ "$(flag_of "$ACCT_JSON" BlockPublicPolicy)" != "true" ]; then
    echo "      Account BPA does not block public policies. If this script"
    echo "      relaxed it on another machine, restore it BY HAND:"
    echo "        aws s3control put-public-access-block --account-id $ACCOUNT \\"
    echo "          --public-access-block-configuration $BPA_SHUT"
  fi
fi

probe
echo
echo "restored:${restored:- nothing}"
echo
echo "Teardown is not finished. Public access is off; the data and the bucket"
echo "are still there and still billing. See docs/aws-public-hls.md:"
echo "  aws s3 rm s3://$BUCKET --recursive"
echo "  aws s3 rb s3://$BUCKET"
