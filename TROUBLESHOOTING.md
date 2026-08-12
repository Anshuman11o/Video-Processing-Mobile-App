# Troubleshooting Log

Running log of non-trivial problems encountered and how they were resolved.
Append new entries as issues arise. Same problem should never be debugged twice.

---

## Template

```markdown
### [Short description]

**Date:** YYYY-MM-DD

**Symptom:** What went wrong, error messages, unexpected behavior.

**Cause:** Why it happened, root cause analysis.

**Fix:** Exact steps to resolve, commands run, code changed.

**Prevention:** How to avoid in the future (if applicable).
```

---

## Issues

### S3 rejects `x-amz-meta-*` in CORS ExposeHeaders

**Date:** 2026-08-12

**Symptom:** `aws s3api put-bucket-cors` against the real `dayreel-raw-videos`
bucket failed with:

```
An error occurred (InvalidRequest) when calling the PutBucketCors operation:
ExposeHeader "x-amz-meta-*" contains wildcard. We currently do not support
wildcard for ExposeHeader.
```

The identical CORS document had been applied to LocalStack on every startup for
days without complaint.

**Cause:** Real S3 does not accept wildcards in `ExposeHeaders` — each exposed
header must be named in full. `AllowedHeaders` does accept `*`, which is what
made the rule look plausible. LocalStack stores the CORS document without
validating it, so the bad rule was accepted locally and only surfaced the first
time it was applied to a real bucket.

**Fix:** Use `"ExposeHeaders": ["ETag"]`. `ETag` is the only response header the
mobile client reads — it persists per-part ETags to resume multipart uploads —
so nothing was actually lost:

```json
{
  "CORSRules": [
    {
      "AllowedOrigins": ["*"],
      "AllowedMethods": ["GET", "PUT", "POST", "DELETE", "HEAD"],
      "AllowedHeaders": ["*"],
      "ExposeHeaders": ["ETag"],
      "MaxAgeSeconds": 3600
    }
  ]
}
```

**Prevention:** This class of bug — emulator accepts, real service rejects — is
the reason LocalStack was dropped from the stack entirely. S3 and DynamoDB now
point at a real AWS account from the first run, so divergences surface
immediately instead of at deploy time. Where an emulator is unavoidable, apply
the same configuration to the real service early rather than trusting that
acceptance locally means acceptance in production.
