# Moderation review and escalation runbook

An escalation is a request for independent review. It never authorizes a payment,
ban, takedown or order-state change. Its expiry default is `HOLD`: preserve the
current marketplace state and take no proposed action.

## Review an item

1. Open `/admin/moderation` through the deployment's existing private
   administrator ingress. Verify that the
   public listener still refuses administrator routes and administrator sessions;
   a claimed `Host` or forwarding header is not private-ingress authentication.
2. Read the disposition, reason codes, suspicion evidence, input/output digests,
   prompt revision, model/weights identity, deadline and default action. Do not
   interpret confidence as authority or evidence of truth.
3. Compare the proposal with the original evidence and current entity state.
   Listing text claiming that an order was delivered does not establish delivery.
   Review a summary for omitted chronology, contradictions and invented facts.
4. Keep hostile instructions as evidence. Do not paste them into an operator
   shell, a coding agent's trusted instructions, a SQL console or another
   administrative action field.
5. **Accept**, **Dismiss** and **Hold** record a review annotation only. Accepting
   a changed or expired item is refused. The form needs the current
   administrator's password and enrolled authenticator, CSRF and submission
   nonce. If a binding action is justified, use the established application workflow
   with its existing authorization and confirmation. Money requires the existing
   independent human or deterministic payment checks. A moderation suggestion
   cannot bypass them.
6. Record the review disposition without changing the evidence. When evidence
   is insufficient, hold or request more evidence through the existing human
   workflow. Leaving an item unattended must not punish its subject.

Repeated reports may share a queue item or pattern. Inspect the underlying
decision references; a report count is not independent corroboration.

## If the model is missing, slow or stopped

The market remains usable. Deterministic checks continue; uncertain items need
human review. No vendor request should await a model call.

Check the administrator-only failure class and last pass. Verify that the local
socket exists, the separately supervised runtime is healthy and the configured
artifact/prompt pins match reviewed values. Keep endpoints, configured paths and
raw runtime errors out of published diagnostics. Do not add a remote API or
download new weights as a fallback.

If a worker stopped mid-item, restart the same worker against the same database
and configuration; its interrupted claim must become available again. Do not
delete a job or edit an order to clear a queue. Reprocessing the same input and
policy must reuse its audit identity.

## If injection suspicion rises

Treat suspicion as a signal, not proof of wrongdoing. Inspect instruction
overrides, delimiter imitation, hidden/control characters, mixed scripts,
oversized fields and model output that names schema fields or attempts to
change trusted facts. False positives are possible, including legitimate
discussion of moderation policy.

Keep high-impact and suspicious proposals in administrator review. If volume
exceeds staffing, stop inference and continue deterministic/human operation;
never lower validation, raise automation authority or use a broad punitive
default to reduce queue depth. Review the bounded worker and queue settings
before restoring inference.

## If the deadline expired

`HOLD` means no proposed action occurs. The original listing/order/account keeps
its existing state. Examine the evidence again before using any existing staff
action. A missed SLA or offline administrator is not consent to release funds,
refund an order, ban an account or publish a generated reply.

## Encrypted offline packets

The durable queue survives an offline administrator even without export
encryption. To additionally create offline packets, give the controller a public
OpenPGP recipient and a separate online Ed25519 report-signing seed:

```sh
./opsecmkt keys generate audit \
  --out /secure/moderation/packet-signer.seed \
  --public-out /public/moderation/packet-signer.pub
```

Keep that seed private to the trusted controller and independent of all other
keys. Transfer its public key to the offline review workstation through an
independently authenticated channel. Add both worker options:

```text
--escalation-recipient /public/moderation/operator-recipient.asc
--signing-key /secure/moderation/packet-signer.seed
```

Their environment alternatives are `MODERATION_ESCALATION_RECIPIENT` and
`MODERATION_SIGNING_KEY_FILE`. Supplying only one is refused. The recipient's
private decryption key stays on the offline workstation. The controller signs
the packet payload, then OpenPGP-encrypts the entire bundle. Without both options
the private queue still works, but no plaintext export substitutes for an
encrypted packet.

Follow the queue's **Download encrypted escalation for offline review** link.
An authenticated administrator can download available ciphertext through
`/admin/moderation/escalation?id=NN`. No packet is sent automatically to a public
onion, email, webhook or external host. Actor/pattern coalescing replaces the
latest encrypted packet for that escalation; individual proposal records remain.

On the offline workstation, decrypt with the matching OpenPGP private key and
verify the Ed25519 signature against the separately pinned worker public key.
For example, with the reviewed repository helpers present offline:

```sh
umask 077
gpg --output packet.json --decrypt moderation-escalation.asc
python3 scripts/verify-moderation-escalation.py --packet packet.json \
  --public-key packet-signer.pub --out verified-payload.json
```

The verifier uses the independent 64-character hex pin in `packet-signer.pub`,
rejects unknown/duplicate fields and invalid bounds, verifies the exact signed
bytes with OpenSSL, then creates a new mode-`0600` payload file. It refuses
overwrite and prints no proposal text or executable instruction. Verification
establishes integrity, including for an expired historical packet; it does not
extend a deadline or authorize an action. Inspect the verified payload as data.

The bundle contains `payload`, hex `signature` and hex `public_key`. Verify the
signature over the exact UTF-8 bytes of the `payload` JSON object; do not
reserialize it and do not trust a new public key solely because it appears in
the bundle. An online controller signing key can be used by a compromised
controller, so the signature does not establish the truth of a proposal.

The payload contains the proposal, deterministic context, evidence digest,
expiry, alternatives and default action. It is not a full copy of original
evidence or a complete dispute timeline. Check independently retained original
evidence before any binding decision.

## Signed review tokens without an account login

An offline signature can authorize only a queue annotation: `accept`, `dismiss`
or `hold`. It cannot authorize an order change, takedown, account restriction or
payment. The token-submission page is `/admin/moderation/offline` on the existing
private administrator ingress. It requires Tor client authorization at that
ingress and a current page CSRF token; it does not require an application
account login. The public listener refuses the page and submission.

Create a separate Ed25519 PEM signing key on the offline workstation; do not
reuse the online packet-signing seed. For example, with an offline copy of
OpenSSL:

```sh
umask 077
openssl genpkey -algorithm ED25519 -out moderation-actions.pem
openssl pkey -in moderation-actions.pem -pubout -outform DER \
  -out moderation-actions-public.der
python3 - <<'PY'
from pathlib import Path
der = Path('moderation-actions-public.der').read_bytes()
assert len(der) == 44 and der[:12].hex() == '302a300506032b6570032100'
Path('moderation-actions.pub').write_text(der[12:].hex() + '\n')
PY
```

Transfer only `moderation-actions.pub` to the application host. Configure
`MODERATION_ACTION_PUBLIC_KEY` to that file and
`MODERATION_ACTION_AUDIENCE` to a newly generated, stable 32-byte random value
encoded as 64 lowercase hex characters. The audience is a public installation
scope, not a secret. Retain the audience and public pin through app redeploys
and recovery; the PEM private key never goes online. Key rotation is an
explicit operator change.

Direct `serve` accepts `--moderation-action-public-key FILE` and
`--moderation-action-audience HEX` as alternatives to those environment
settings. For a **new** managed Tor installation, add
`--moderation-action-public-key FILE` and
`--set MODERATION_ACTION_AUDIENCE=HEX` to the reviewed `deploy init` command.
Both values are required together, along with the private administrator onion
configuration. The controller imports the public pin read-only and preserves
it and the audience through app-only redeployments. Do not rerun `init` on an
existing installation to add or rotate a pin.

Prepare and inspect a `token.json` object with exactly these fields, replacing
the illustrative placeholders with verified item values:

```json
{
  "version": 1,
  "audience": "INSTALLATION_AUDIENCE_64_LOWERCASE_HEX",
  "suggestion_id": "VERIFIED_SUGGESTION_ID_64_LOWERCASE_HEX",
  "content_hash": "VERIFIED_EVIDENCE_DIGEST_64_LOWERCASE_HEX",
  "action": "hold",
  "expires_at": "YYYY-MM-DDTHH:MM:SSZ",
  "nonce": "FRESH_RANDOM_NONCE_64_LOWERCASE_HEX"
}
```

Use a fresh random nonce, for example from `openssl rand -hex 32`. Expiry must be
in UTC, in the future, no more than seven days away and no later than the review
item's own expiry. A suggestion ID or evidence digest must come from the
verified packet/current queue, never from a model's prose. Sign using the
repository's reviewed helper on the offline workstation:

```sh
python3 scripts/sign-moderation-action.py --token token.json \
  --key moderation-actions.pem --out signed-review.json
```

The helper uses a private temporary, seekable message file for OpenSSL's
Ed25519 operation; it does not copy the private key and removes the temporary
message afterward. It signs the compact token bytes prefixed by
`opsecmkt-moderation-action-v1` and a newline, then creates a new mode-`0600`
envelope. Do not edit that envelope after signing. Transfer only the signed
envelope to the authorized client and paste it into the private page. The
application checks signature, installation audience, exact fields, current
entity/content binding, expiry and one-use nonce/item receipts in one
transaction. Replay and already-reviewed items are refused. A successfully
recorded token still leaves marketplace state and funds unchanged.

## Evidence and recovery

Preserve authentic UTC timestamps, digests and originals under the deployment's
access and preservation policy. Share only a reviewed, sanitized copy. Include
moderation tables and configuration in the database/installation recovery plan;
do not silently drop them during a restore. Retain keys, entity revisions and
current payment-recovery gates as required by the existing recovery workflow.

The [design](moderation-agent-design.md) describes the trust boundary and
additional data risks. The [operational checklist](signature-hygiene.md) separates
adapter guarantees from actual runtime isolation.
