# Moderation operational hygiene

This checklist limits accidental disclosure and unnecessary attack surface from
the moderation service. It does not provide anonymity, defeat traffic analysis,
evade lawful process or conceal changes in control of a deployment. The
[design](moderation-agent-design.md) records the additional risks introduced by
inference and retained moderation records.

## Before enabling inference

- Keep the model disabled until its runtime, weights, license, resource budget
  and isolation have been reviewed. Verify pinned artifacts through an
  independent trusted source; a hash alone is not a safety assessment.
- Acquire artifacts during a separate reviewed maintenance step. Runtime
  moderation must not download models, update packages, send telemetry or check
  a model hub.
- Use the configured local Unix socket. Add no clearnet port, public model
  endpoint, arbitrary fetch tool, remote inference API or automatic webhook.
- Give the inference process no application environment, database role, wallet
  credentials, Tor identity, administrator keys, Docker socket or secret mounts.
  Check actual mounts, namespaces, UID and network policy on the deployed host.
- Bound CPU, memory, temporary storage, request duration, input and output size,
  queue batches and retries. Verify the market remains responsive when inference
  is absent or stopped during a request.
- Keep original image decoding on the existing upload gateway. An image digest
  is not evidence that a model reviewed the image or verified a listing claim.

## Administrator and public surfaces

- Keep runtime identity, prompt revision, weights digest, failure classes and
  queue diagnostics on the existing authorized administrator surface. Do not
  print configured hosts, paths, URLs, credentials or raw transport errors.
- Use existing server-rendered HTML, CSRF and authorization patterns. Add no
  browser JavaScript, third-party assets, web fonts, public model badge or
  provider-specific error response.
- Escape every model-generated string as ordinary text. A suggested action,
  URL, instruction or HTML fragment is data, including when a model claims that
  an administrator approved it.
- Preserve existing error handling and public assets unless the product change
  itself needs an asset update. Do not change unrelated favicons, schedules or
  public canary procedures as a side effect of moderation work.
- Make no claim that a visitor cannot distinguish human and model work by timing,
  content or page shape. Queue processing reduces request coupling but does not
  prove indistinguishability.

## Review and records

- Keep generated summaries as review aids. Verify original evidence and current
  order state before using the established human action workflow.
- Do not automatically publish model replies. If publication is added later,
  review the text for factual accuracy, stable terminology, unnecessary model
  identity, emoji, local-time assumptions and unsupported claims. Store
  moderation event times in UTC.
- State every queue item's expiry and `HOLD` default. A missed review deadline
  must not punish a user or authorize money movement.
- Preserve authentic audit times, evidence and required holds. Review and
  sanitize a separate copy before sharing logs, screenshots or exports.
- Include moderation records in access, capacity, backup and retention review.
  Do not infer automatic deletion from unrelated retention jobs.
- Preserve the existing independently signed canary and its verification
  procedure. A model canary token checks response shape; it is not a human
  statement, an authentication credential or evidence of who controls a host.

Use the [escalation runbook](moderation-escalation-runbook.md) for review and
failure handling. Record real deployed isolation checks separately from unit
tests; a source-level default cannot attest a running host.
