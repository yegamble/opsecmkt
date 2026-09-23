"""The completion reminder must request review without trapping Claude."""
import json
from pathlib import Path
import subprocess
import unittest

ROOT = Path(__file__).resolve().parents[1]
HOOK = ROOT / '.claude/hooks/scrutinise.py'


class ReviewHookTests(unittest.TestCase):
    def invoke(self, payload):
        result = subprocess.run(
            ['python3', str(HOOK)], input=payload, text=True,
            capture_output=True, timeout=5, check=True,
        )
        self.assertEqual(result.stderr, '')
        return json.loads(result.stdout) if result.stdout else None

    def test_main_and_subagent_stop_request_review(self):
        for event in ('Stop', 'SubagentStop'):
            with self.subTest(event=event):
                result = self.invoke(json.dumps({
                    'hook_event_name': event, 'stop_hook_active': False,
                }))
                self.assertEqual(result['decision'], 'block')
                self.assertIn('.claude/review-loop.md', result['reason'])
                self.assertIn('Read-only reviewers', result['reason'])

    def test_hook_continuation_can_stop(self):
        for event in ('Stop', 'SubagentStop'):
            with self.subTest(event=event):
                self.assertIsNone(self.invoke(json.dumps({
                    'hook_event_name': event, 'stop_hook_active': True,
                })))

    def test_malformed_or_unrelated_input_fails_open(self):
        for payload in ('', '{', 'null', '[]', '{}',
                        '{"hook_event_name":"PreToolUse"}'):
            with self.subTest(payload=payload):
                self.assertIsNone(self.invoke(payload))

    def test_settings_wire_both_events_to_existing_hook(self):
        hooks = json.loads((ROOT / '.claude/settings.json').read_text())['hooks']
        for event in ('Stop', 'SubagentStop'):
            command = hooks[event][0]['hooks'][0]
            self.assertEqual(command['type'], 'command')
            self.assertEqual(command['command'],
                             'python3 "$CLAUDE_PROJECT_DIR/.claude/hooks/scrutinise.py"')
            self.assertTrue(HOOK.is_file())


if __name__ == '__main__':
    unittest.main()
