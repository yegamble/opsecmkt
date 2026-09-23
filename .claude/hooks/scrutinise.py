#!/usr/bin/env python3
"""Request one final scrutiny pass; never trap a session in a stop-hook loop.

Protocol: https://code.claude.com/docs/en/hooks#stop-decision-control
The hook is a review reminder, not proof that tests or review passed.
"""
import json
import sys


def decision(payload):
    if not isinstance(payload, dict):
        return None
    if payload.get("hook_event_name") not in ("Stop", "SubagentStop"):
        return None
    if payload.get("stop_hook_active") is True:
        return None
    return {
        "decision": "block",
        "reason": (
            "Before finishing, scrutinise your assigned OPSEC Market scope once more using "
            ".claude/review-loop.md. Re-read the request and final diff, challenge the completion "
            "claim with concrete evidence, and check relevant failure paths and missing tests. "
            "If you find an in-scope defect, fix it when authorised, verify it, and repeat the "
            "review until no required finding remains. Read-only reviewers report findings to "
            "their parent instead of editing. Report passed, failed and skipped checks honestly. "
            "Do not broaden scope, manufacture work, publish changes, or touch unrelated edits. "
            "If the user asked you to stop, a required answer is missing, or progress is blocked, "
            "respect that and report the specific blocker. This reminder runs once per stop "
            "continuation; it does not certify completion."
        ),
    }


def main():
    try:
        result = decision(json.load(sys.stdin))
    except (ValueError, OSError):
        # Malformed input must not strand the user in a broken hook.
        return
    if result:
        print(json.dumps(result))


if __name__ == "__main__":
    main()
