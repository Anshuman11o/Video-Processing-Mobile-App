# Public read on the HLS bucket, on real AWS

> **If you turn this on, turn it back off.** `./scripts/aws-hls-public.sh disable --yes`
>
> It is the single thing most likely to be forgotten, and it is the only step
> here that leaves a real AWS account worse than it found it.

This document covers the **optional, opt-in, off-by-default** public-read access
model for `dayreel-hls-output` on real AWS: why presigning cannot serve HLS, what
the script changes, what Block Public Access actually does at each level, what it
costs, how to tear it down, and the alternative that was deliberately not built.

Nothing in this document is on by default. The `dayreel-hls-output` bucket has
never had a public-read access model applied to it, and no step in this document
has been executed against a real bucket. Read
[VERIFIED vs ASSUMED](#verified-vs-assumed) at the bottom before acting on any of
it.

(This paragraph used to say no real AWS bucket had ever been provisioned for the
project at all. That stopped being true on 2026-08-12, when the three buckets and
the jobs table were created by hand — the CORS failure recorded in
`TROUBLESHOOTING.md` happened against the real `dayreel-raw-videos`. The buckets
exist; what remains unexercised is everything below.)

---

## Why presigning cannot serve HLS

An HLS master playlist points at variant playlists, and each variant playlist
points at its segments — **by relative path**:

```
#EXTM3U
#EXT-X-VERSION:6
#EXTINF:4.000,
segment_000.ts        <- relative. No signature, nowhere to put one.
```

Presign the master and the player fetches it fine, then resolves
`480p/playlist.m3u8` against the master's URL, drops the query string, and gets a
`403` — and the same again for every segment. Presigning therefore cannot deliver
HLS **unless every URI in every playlist is individually rewritten**, which is the
alternative discussed [below](#the-alternative-that-was-not-built).

### Why this was never visible locally

**LocalStack Community served unsigned GETs to any bucket.** Playback worked on
the emulator for a reason that will not hold on real S3 — not because the access
model was right, but because there was no access model. It accepted
`put-bucket-policy` and `put-public-access-block`, read both back verbatim, and
enforced neither, so provisioning code that set them looked correct while proving
nothing.

The emulator is gone, and so is the init script that would otherwise have been
the tempting place to configure this. What the episode leaves behind is the
reason this document is opt-in and manual: the access model is a one-time
decision on a real, billed account, and the local stack never had any way to
tell you whether you had got it right. `docs/SETUP.md`, under "Bucket
authorization has to be asserted, not assumed", carries the same lesson for the
upload path.

---

## What the script does

```bash
./scripts/aws-hls-public.sh status                                # read-only
./scripts/aws-hls-public.sh status --probe-key <job-uuid>/master.m3u8
./scripts/aws-hls-public.sh enable                                # DRY RUN
./scripts/aws-hls-public.sh enable  --yes                         # mutates
./scripts/aws-hls-public.sh disable --yes                         # reverses
```

Without `--yes` every path is a dry run that prints the exact calls it would make.
The script refuses to run against LocalStack at all — both if `AWS_ENDPOINT_URL` is
set and if the resolved account is `000000000000` — because a green run there would
mean nothing.

### The exact AWS API calls

`enable`, in this order. **The order is load-bearing:** `BlockPublicPolicy=true`
makes `PutBucketPolicy` reject a public policy outright, so the guard has to come
down before the policy goes up.

```bash
# 0. read-only, first
aws sts get-caller-identity --query Account --output text
aws s3api head-bucket              --bucket dayreel-hls-output
aws s3control get-public-access-block --account-id <account>       # may 404; see below
aws s3api get-public-access-block  --bucket dayreel-hls-output
aws s3api get-bucket-policy        --bucket dayreel-hls-output
aws s3api get-bucket-policy-status --bucket dayreel-hls-output

# 1. ONLY if account-level BPA blocks public policies, and only behind the
#    second flag --allow-account-bpa-change. ACL flags are preserved as found.
aws s3control put-public-access-block --account-id <account> \
  --public-access-block-configuration \
    BlockPublicAcls=<as-found>,IgnorePublicAcls=<as-found>,BlockPublicPolicy=false,RestrictPublicBuckets=false

# 2. bucket level. Note the ACL guards stay ON — access is granted by policy,
#    not by ACL, so there is no reason to relax them.
aws s3api put-public-access-block --bucket dayreel-hls-output \
  --public-access-block-configuration \
    BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=false,RestrictPublicBuckets=false

# 3. the policy
aws s3api put-bucket-policy --bucket dayreel-hls-output --policy '<see below>'
```

`disable`, in this order. **Policy first**, so that a partial run leaves the bucket
private rather than exposed:

```bash
aws s3api delete-bucket-policy   --bucket dayreel-hls-output
aws s3api put-public-access-block --bucket dayreel-hls-output \
  --public-access-block-configuration \
    BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
aws s3control put-public-access-block --account-id <account> \
  --public-access-block-configuration <exactly what enable recorded>
```

`enable` writes what it displaced to `$TMPDIR/dayreel-hls-public.<bucket>.json`
and **also prints it to stdout**, so a lost temp file does not cost you the
account-level restore. Re-running `enable` does not overwrite an existing record —
otherwise the second run would save the already-open state and `disable` would
faithfully "restore" the bucket to public.

---

## The bucket policy

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "PublicReadHLSObjects",
      "Effect": "Allow",
      "Principal": "*",
      "Action": "s3:GetObject",
      "Resource": "arn:aws:s3:::dayreel-hls-output/*"
    }
  ]
}
```

### Why `s3:GetObject` on `/*`, and deliberately NOT `s3:ListBucket`

> **This is the most important detail in this document.**

`s3:GetObject` on `arn:aws:s3:::dayreel-hls-output/*` grants reads only to a caller
who **already knows the full key**. The keys are

```
<job_id>/master.m3u8
<job_id>/<rendition>/playlist.m3u8
<job_id>/<rendition>/segment_NNN.ts
```

and `job_id` is a UUID (`OutputKey` and the upload loop in
`backend/internal/worker/packager/packager.go`). An anonymous caller needs 122 bits
of secret to read one reel. Guessing gets them nothing.

`s3:ListBucket` is a **different action on a different resource** — the bare bucket
ARN `arn:aws:s3:::dayreel-hls-output`, with no `/*`. Adding it would let anyone on
the internet call `ListObjectsV2` and enumerate **every job the pipeline has ever
processed**, converting the unguessable key into a published index. The secrecy of
the key is the only thing separating "readable by the person who made it" from
"readable by everyone", so `ListBucket` must never appear here.

Its absence is a decision, not an oversight and not a hardening TODO. The same goes
for the bare bucket ARN never appearing in `Resource`.

**What this still does not give you:** anyone who obtains a reel URL — from a log,
a screenshot, a shared link, a referrer header — can read that reel, forever, until
the policy comes off. Unguessable is not the same as private.

---

## Block Public Access: the part that is easy to get wrong

S3 Block Public Access (BPA) is four independent boolean settings:

| Setting | Effect |
|---|---|
| `BlockPublicAcls` | Reject `PutBucketAcl`/`PutObjectAcl`/`PutObject` calls carrying a public ACL |
| `IgnorePublicAcls` | Ignore public ACLs that already exist |
| `BlockPublicPolicy` | **Reject `PutBucketPolicy` if the policy is public** |
| `RestrictPublicBuckets` | **Restrict a bucket that has a public policy to AWS service principals and same-account callers** |

The bottom two are the ones this feature collides with. The policy above **is**
public by S3's own definition: a policy granting `"Principal": "*"` with no limiting
`Condition` is public, which is why `BlockPublicPolicy` rejects it and
`RestrictPublicBuckets` neutralises it.

### It applies at four levels, and the most restrictive one wins

BPA can be set on an **AWS Organizations policy**, an **account**, a **bucket**, and
an **access point**. From the AWS S3 User Guide:

> "If the block public access settings for the access point, bucket, or account
> differ, then Amazon S3 applies the most restrictive combination of the access
> point, bucket, and account settings."

So **a permissive bucket under a locked-down account is still locked down.** If
account-level `BlockPublicPolicy`/`RestrictPublicBuckets` are on, relaxing the
bucket alone changes nothing — the `PutBucketPolicy` is rejected, or the policy
applies and is then ignored. That is the "it looks like it worked but the object is
still 403" failure, and it is the reason the script reads the account level before
it decides anything.

### But account-level BPA is **not** on by default — check, do not assume

This is where the received wisdom is wrong, and it changes the blast radius
substantially.

| | On by default? |
|---|---|
| **Bucket-level BPA**, buckets created since **April 2023** | **Yes** — all four settings on, for all new buckets, however created. Existing buckets were not changed. |
| **Account-level BPA** | **No.** It is opt-in. An account that has never configured it has no configuration at all. |

`aws s3control get-public-access-block` against such an account returns **HTTP 404
`NoSuchPublicAccessBlockConfiguration`** — "Amazon S3 throws this exception if you
make a `GetPublicAccessBlock` request against an account that doesn't have a
`PublicAccessBlockConfiguration` set." Nothing about creating a bucket sets it.

**The consequence for this project:** on a plain single account that has never had
account-level BPA configured — the likely case here — making this one bucket
readable requires **only the bucket-level change**. No account-wide guardrail has to
come off, because none is on. The script reports `account BPA : NONE` and skips
step 1 entirely.

**When the account-wide blast radius is real:** if account-level BPA *is* set — by a
person, by Control Tower, by a Security Hub remediation, or inherited from an
Organizations S3 BPA policy — then relaxing it is genuinely account-wide. Turning
off `BlockPublicPolicy` and `RestrictPublicBuckets` at the account level **removes
that guardrail from every bucket in the account, for as long as it is off** — every
bucket becomes one careless `put-bucket-policy` away from public, not just this one.
That is far larger than the one bucket, and it is why the script puts that step
behind a **second** flag (`--allow-account-bpa-change`) rather than letting it ride
along on `--yes`.

**And sometimes you cannot do it at all:** if an AWS Organizations BPA policy is in
force, account-level `PutPublicAccessBlock` and `DeletePublicAccessBlock` fail with
`AccessDenied` — "This account does not allow changes to its account-level S3 Block
Public Access settings due to an organizational S3 Block Public Access policy in
effect." There is no workaround from inside the member account, and there should not
be. If you hit this, use the [alternative](#the-alternative-that-was-not-built).

### Turning BPA back on does not undo the policy

> "Block public access settings don't alter existing policies or ACLs. Therefore,
> removing a block public access setting causes a bucket or object with a public
> policy or ACL to again be publicly accessible."

Re-enabling BPA **masks** the public policy; it does not delete it. A bucket left in
that state is one `put-public-access-block` away from being public again, silently,
with no one having written a policy. This is why `disable` deletes the policy **and**
restores BPA, and why the teardown checklist below has four items rather than two.

---

## TURN IT BACK ON

```bash
./scripts/aws-hls-public.sh disable --yes
```

A bucket left public is not a bug that surfaces. Nothing fails, no test goes red, no
alarm fires until the bill does. `config/free-tier.md` already states the rule this
falls under: *no AWS resource should outlive the run that needed it*, and whoever
provisions something must say so explicitly at the end of that run. A public-read
policy is exactly such a thing — it is a standing grant to the entire internet, and
it is invisible from every part of this repo.

Confirm with the only check that measures what anyone cares about — an
unauthenticated GET, which is what a stranger's player is:

```bash
curl -s -o /dev/null -w '%{http_code}\n' \
  https://dayreel-hls-output.s3.us-east-1.amazonaws.com/<job-uuid>/master.m3u8
# 403 = shut. 200 = still open.
```

Reading the configuration back from the API is *not* the same check. That is the
class of evidence that has been wrong in this project twice.

---

## Cost exposure

**Legitimate use is effectively free.** A 10-second reel is a few MB across a
handful of objects; `config/free-tier.md` puts S3 GET at ~$0.0004/1,000 requests and
notes egress is billed at $0.09/GB only **after the first 100 GB/month**. That 100 GB
allowance is permanent and account-wide — it is not part of the 12-month new-account
free tier, so it applies here despite this account having no free tier. Playing the
reel a few hundred times rounds to $0.00.

**The risk is not legitimate use.** A public bucket bills the *owner* for every GET
and every byte egressed, from anyone, forever. A discovered-and-scraped public bucket
is the classic S3 bill-shock story, and this project's total budget is **$20** — small
enough that a single sustained scrape ends it. Egress past 100 GB at $0.09/GB reaches
$20 at roughly 322 GB.

The `GetObject`-only policy is what keeps this from being a real exposure: without
`ListBucket` there is no index to crawl, so a scraper has nothing to enumerate. That
is a second, independent reason `ListBucket` is absent — it is a cost control as well
as a privacy control.

**Set a budget alarm before enabling, not after.** `config/free-tier.md` already
specifies the thresholds against the $20 ceiling — $2 / $5 / $10 / $20 — and the
`aws budgets create-budget` invocation. Alarms are free; they are the only thing that
catches a mistake while it is still small.

---

## Teardown checklist

All four. Stopping after two leaves either a live public grant or a live bill.

- [ ] **1. Delete the bucket policy** — `./scripts/aws-hls-public.sh disable --yes`,
      or `aws s3api delete-bucket-policy --bucket dayreel-hls-output`.
      Masking it with BPA is not deleting it.
- [ ] **2. Re-enable Block Public Access, bucket *and* account.** `disable` does the
      bucket unconditionally and the account only if `enable` changed it, restoring
      exactly what it recorded. If the state file is gone, set the account level by
      hand — it is account-wide, so do not leave it to chance.
- [ ] **3. Delete the objects** — `aws s3 rm s3://dayreel-hls-output --recursive`.
      They bill as storage regardless of the access model.
- [ ] **4. Delete the bucket** — `aws s3 rb s3://dayreel-hls-output`.
      An empty bucket is free, but it is also a name that outlives the project and
      can be made public again by anyone with the credentials.

Then re-run the generic sweep in `config/free-tier.md` — this bucket is not the only
thing a real-AWS run leaves behind.

---

## The alternative that was not built

**Presign every segment URI by post-processing ffmpeg's variant playlists.**
The bucket stays private, bucket-level BPA stays fully on, and account-wide BPA is
never touched at all. **This is not implemented.** It is recorded here so the choice
is visible and costed, not because it is queued.

### What it would actually involve

The playlists come from two different places, and only one of them is ours:

- **The master playlist is generated by this repo** — `MasterPlaylist()` in
  `backend/internal/media/playlist.go`. Its variant URIs and the `EXT-X-MEDIA`
  subtitle `URI` are strings we already control, so presigning those is a change to
  code we own.
- **The variant playlists are generated by ffmpeg** — via `-hls_segment_filename`
  in `backend/internal/media/hls.go`, which writes
  `<outDir>/%v/segment_%03d.ts` alongside `<outDir>/%v/playlist.m3u8`. Their segment
  URIs are ffmpeg's output. Presigning them means **reading each variant playlist
  back after ffmpeg exits and rewriting every `segment_NNN.ts` line** before upload —
  a post-processing pass over generated files, in the packager stage.

The subtitle playlist (`SubtitlePlaylist()`, same file) would need the same treatment
for its VTT URI.

### What it costs

- **Much larger playlists.** A presigned URL is several hundred characters against
  roughly fifteen for `segment_000.ts`, and every segment in every rendition carries
  one. At 10-second clips this is trivial; it grows linearly with duration and with
  ladder height, and the playlist has to be fetched before the first frame plays.
- **Presigned URLs expire.** One hour is ample for a 10-second clip. For a long
  video it is a **mid-playback failure**: the playlist is fetched once at the start,
  so a viewer still watching at the 61st minute gets 403s on segments whose URLs
  died while they were watching, and the player reports it as a network error. There
  is no renewal mechanism in the format — fixing it properly means re-fetching
  playlists, which means a server that reissues them.
- **It moves the complexity into the pipeline**, where a mistake produces a reel that
  plays for the developer (whose URLs are fresh) and fails for everyone else later.
  That is the exact silent-failure shape this project keeps getting caught by.

### The third option, for completeness

**CloudFront with an Origin Access Control** in front of a private bucket is the
actual production answer. It is not pursued here for two
reasons: CloudFront is LocalStack **Pro-only** (`config/free-tier.md`, parity table),
so it cannot be exercised locally at all; and a distribution is one more real resource
to remember to tear down, against a $20 budget and a project with a handful of runs
left.

---

## VERIFIED vs ASSUMED

Stated plainly, because in this project every significant bug so far compiled, passed
its tests, and showed a green happy path — and twice a claim taken from AWS docs and
never tested turned out wrong.

### VERIFIED

- **The AWS CLI command names and argument shapes** in the script and in this
  document, checked against the AWS CLI installed on this machine (`aws-cli/2.36.21`):
  `s3api put-bucket-policy` / `delete-bucket-policy` / `get-bucket-policy` /
  `get-bucket-policy-status` / `put-public-access-block` / `get-public-access-block` /
  `delete-public-access-block`, and `s3control put-public-access-block` /
  `get-public-access-block` / `delete-public-access-block` with `--account-id`.
- **The BPA semantics quoted above**, against the current AWS S3 User Guide and API
  reference: the four settings, the most-restrictive-combination rule, the four
  levels, `NoSuchPublicAccessBlockConfiguration` as the account-level 404, the
  Organizations override and its `AccessDenied`, and that BPA masks rather than
  deletes an existing policy.
- **That the April 2023 "on by default" change is bucket-level, not account-level**,
  and that existing buckets were not changed.
- **The script's own control flow** — argument parsing, the usage banner, the
  `AWS_ENDPOINT_URL` refusal, the JSON helpers, and the state-file round trip that
  `disable` depends on — exercised directly on this machine.
- **The key layout and the UUID job id** that the `GetObject`-only argument rests on,
  read from `backend/internal/worker/packager/packager.go`.
- **That the variant playlists are ffmpeg's and the master is ours**, read from
  `backend/internal/media/hls.go` and `playlist.go`.

### ASSUMED

- **That the script has the intended effect on real S3.** It has never been run
  against a real AWS account. The buckets themselves do exist as of 2026-08-12,
  but no access model has been applied to `dayreel-hls-output` and spending is
  budgeted at $20 total, so it was not run to find out. The effect of `enable` —
  that an anonymous GET returns 200 afterwards — and of `disable` — that it
  returns 403 again — is **unverified**.
- **That the emulator could never have substituted for that verification.** This
  was not a gap effort would have closed: LocalStack Community **stored bucket
  policies and public-access-block settings and then ignored them entirely** —
  verified twice in this project and recorded in `docs/SETUP.md`. Both calls
  succeeded, both read back verbatim, and an unsigned request still succeeded.
  Now that the emulator is gone there is no local substitute at all, which is
  clearer if less convenient: the only way to know is to run it once.
  Running the script against LocalStack would print an unbroken column of successes
  and prove nothing, which is why the script refuses to run there.
- **That this account has no account-level BPA and is not in an Organizations BPA
  policy.** Not checked — checking it means calling a real account. `status` reports
  the truth in about a second; run it before assuming either way.
- **The cost figures**, which are carried over from `config/free-tier.md` rather than
  re-derived, and which are list prices in one region.

**The first real-AWS run is the verification.** Do it in this order: `status` (expect
`account BPA : NONE`), `enable --yes`, `status --probe-key <job>/master.m3u8` (expect
`200`), play the reel, `disable --yes`, probe again (expect `403`), then the teardown
checklist. Record what actually happened here — including anything above that turns
out to be wrong.
