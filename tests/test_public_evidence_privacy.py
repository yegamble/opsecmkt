"""Keep manually published cloud evidence free of provider identity metadata."""

import json
from pathlib import Path
import unittest


ROOT = Path(__file__).resolve().parent.parent
EVIDENCE = ROOT / "docs/evidence/public-testnet"

# New record kinds or provider fields require a publication review. Proof manifests
# and transcripts have their own runner validation; these are supplemental records.
PUBLIC_FIELDS = {
    "cloud-resource.json": {
        "created_at", "observed_at", "size", "size_slug", "hourly_usd",
        "price_hourly_usd", "disk_gb", "memory_mb", "vcpus", "volume_ids",
        "features", "deleted", "cost_note", "cleanup_recorded_at",
        "identity_verified_before_cleanup", "redaction",
    },
    "cloud-storage.json": {"observed", "bitcoin_datadir_bytes", "scope", "redaction"},
    "cloud-cleanup.json": {
        "absence_verified_at", "deleted", "final_private_recovery_saved_and_verified",
        "known_unrelated_droplets_still_present", "no_blockchain_files_downloaded",
        "public_evidence_saved_and_verified", "recovery_reopened_in_separate_container",
        "verification", "redaction",
    },
}


def privacy_errors(filename, record):
    if filename not in PUBLIC_FIELDS:
        return ["unreviewed cloud record kind"]
    if not isinstance(record, dict):
        return ["cloud record must be an object"]
    errors = []
    if set(record) - PUBLIC_FIELDS[filename]:
        errors.append("unreviewed cloud metadata fields")
    if record.get("volume_ids"):
        errors.append("provider volume identifiers must be omitted")
    if "redaction" in record:
        redaction = record["redaction"]
        if (not isinstance(redaction, dict) or set(redaction) != {"fields", "reason"}
                or not isinstance(redaction.get("reason"), str) or not redaction["reason"].strip()
                or not isinstance(redaction.get("fields"), list) or not redaction["fields"]
                or any(not isinstance(field, str) or not field for field in redaction["fields"])):
            errors.append("redaction must list omitted fields and explain why")
        elif any(field in record for field in redaction["fields"]):
            errors.append("redacted fields are still present")
    return errors


class PublicEvidencePrivacyTests(unittest.TestCase):
    def test_published_cloud_records_contain_only_reviewed_fields(self):
        records = sorted(EVIDENCE.rglob("cloud-*.json"))
        self.assertTrue(records, "public cloud evidence was not found")
        for path in records:
            with self.subTest(record=str(path.relative_to(ROOT))):
                self.assertEqual(privacy_errors(path.name, json.loads(path.read_text())), [])

    def test_refuses_unreviewed_identifiers_and_inaccurate_redactions(self):
        for field in ("id", "name", "tags", "public_ipv4", "region", "account_id", "vpc_uuid",
                      "ssh_fingerprint", "bucket_name"):
            with self.subTest(field=field):
                self.assertTrue(privacy_errors("cloud-resource.json", {field: "test-value"}))
        self.assertTrue(privacy_errors("cloud-resource.json", {"volume_ids": [123]}))
        self.assertTrue(privacy_errors("cloud-credentials.json", {}))
        self.assertTrue(privacy_errors("cloud-cleanup.json", {
            "redaction": {"fields": ["deleted"], "reason": "test"}, "deleted": True,
        }))
        self.assertTrue(privacy_errors("cloud-cleanup.json", {
            "redaction": {"fields": ["droplet_id"], "reason": ""},
        }))


if __name__ == "__main__":
    unittest.main()
