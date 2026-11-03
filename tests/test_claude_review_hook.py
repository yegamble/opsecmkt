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

    def transcript(self, *rows):
        import tempfile
        f = tempfile.NamedTemporaryFile('w', suffix='.jsonl', delete=False)
        self.addCleanup(Path(f.name).unlink)
        for row in rows:
            f.write((row if isinstance(row, str) else json.dumps(row)) + '\n')
        f.close()
        return f.name

    PROMPT = {'type': 'user', 'message': {'role': 'user', 'content': 'fix the order page'}}
    NOTICE = {'type': 'user', 'message': {'role': 'user',
              'content': '<task-notification><task-id>b1</task-id></task-notification>'}}
    TOOL = {'type': 'user', 'message': {'role': 'user', 'content': [{'type': 'tool_result', 'content': 'ok'}]}}
    META = {'type': 'user', 'isMeta': True, 'message': {'role': 'user', 'content': 'Stop hook feedback: …'}}

    def reminder(self, event='Stop'):
        return {'type': 'attachment', 'attachment': {
            'type': 'hook_blocking_error', 'hookEvent': event,
            'blockingError': {'blockingError': 'Before finishing, scrutinise your assigned OPSEC Market scope once more'}}}

    def stop(self, path, event='Stop'):
        return self.invoke(json.dumps({'hook_event_name': event, 'stop_hook_active': False, 'transcript_path': path}))

    def test_stop_reminds_once_per_typed_request(self):
        # The first stop after a request is reminded.
        self.assertEqual(self.stop(self.transcript(self.PROMPT, self.TOOL))['decision'], 'block')
        # Later stops on the same request (background notices, tool results, the hook's own feedback) are not.
        path = self.transcript(self.PROMPT, self.reminder(), self.META, self.NOTICE, self.TOOL)
        self.assertIsNone(self.stop(path))
        # A new request is reminded again.
        path = self.transcript(self.PROMPT, self.reminder(), self.NOTICE,
                               {'type': 'user', 'message': {'role': 'user', 'content': [{'type': 'text', 'text': 'push it'}]}})
        self.assertEqual(self.stop(path)['decision'], 'block')

    def test_other_reminders_and_subagents_do_not_suppress(self):
        # A reminder before the latest request, or a SubagentStop reminder, does not count.
        self.assertEqual(self.stop(self.transcript(self.reminder(), self.PROMPT))['decision'], 'block')
        self.assertEqual(self.stop(self.transcript(self.PROMPT, self.reminder('SubagentStop')))['decision'], 'block')
        # SubagentStop keeps its one reminder per continuation regardless of the transcript.
        path = self.transcript(self.PROMPT, self.reminder())
        self.assertEqual(self.stop(path, 'SubagentStop')['decision'], 'block')

    def test_unreadable_transcript_still_reminds(self):
        for path in (None, '', '/nonexistent/transcript.jsonl', self.transcript('{', 'null', '[]')):
            with self.subTest(path=path):
                self.assertEqual(self.stop(path)['decision'], 'block')

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
