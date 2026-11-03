#!/usr/bin/env python3
"""Request one final scrutiny pass; never trap a session in a stop-hook loop.

Protocol: https://code.claude.com/docs/en/hooks#stop-decision-control
The hook is a review reminder, not proof that tests or review passed.
"""
import json
import sys

REASON_PREFIX = "Before finishing, scrutinise your assigned OPSEC Market scope"


def _real_prompt(row):
    """A message the user typed, not a tool result, hook feedback or background notification."""
    if row.get("type") != "user" or row.get("isMeta"):
        return False
    content = row.get("message", {}).get("content") if isinstance(row.get("message"), dict) else None
    if isinstance(content, list):
        if any(isinstance(b, dict) and b.get("type") == "tool_result" for b in content):
            return False
        content = " ".join(b.get("text", "") for b in content if isinstance(b, dict))
    return isinstance(content, str) and not content.lstrip().startswith("<task-notification")


def _stop_reminder(row):
    att = row.get("attachment")
    if not isinstance(att, dict) or att.get("type") != "hook_blocking_error" or att.get("hookEvent") != "Stop":
        return False
    err = att.get("blockingError")
    text = err.get("blockingError") if isinstance(err, dict) else err
    return isinstance(text, str) and text.startswith(REASON_PREFIX)


def already_reminded(transcript_path):
    """True when a Stop reminder already follows the latest prompt the user typed.

    Background notifications end a turn without a new request; reminding on each one repeats the same
    review. Any doubt (no path, unreadable file, no prompt found) answers False, so the reminder runs."""
    if not isinstance(transcript_path, str) or not transcript_path:
        return False
    reminded = None
    try:
        with open(transcript_path, encoding="utf-8") as f:
            for line in f:
                if '"user"' not in line and "hook_blocking_error" not in line:
                    continue
                try:
                    row = json.loads(line)
                except ValueError:
                    continue
                if not isinstance(row, dict):
                    continue
                if _real_prompt(row):
                    reminded = False
                elif reminded is False and _stop_reminder(row):
                    reminded = True
    except OSError:
        return False
    return reminded is True


def decision(payload):
    if not isinstance(payload, dict):
        return None
    if payload.get("hook_event_name") not in ("Stop", "SubagentStop"):
        return None
    if payload.get("stop_hook_active") is True:
        return None
    if payload.get("hook_event_name") == "Stop" and already_reminded(payload.get("transcript_path")):
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
            "respect that and report the specific blocker. This reminder runs once per request "
            "you were given; it does not certify completion."
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
