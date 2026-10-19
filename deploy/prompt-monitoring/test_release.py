import argparse
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import release


class ReleaseSafetyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.folder = self.base / 'backups' / 'test'
        self.folder.mkdir(parents=True)
        self.args = argparse.Namespace(base=self.base, release=self.folder)
        (self.base / 'config.json').write_text('{"listen":"127.0.0.1:7863"}')
        (self.base / release.BINARY).write_bytes(b'old binary')
        (self.folder / 'workbuddy2api.before').write_bytes(b'old binary')
        (self.folder / 'workbuddy2api.next').write_bytes(b'new binary')
        (self.folder / 'config.before.json').write_bytes((self.base / 'config.json').read_bytes())
        (self.folder / 'archives-before').mkdir()
        (self.folder / 'archives-before' / 'manifest.json').write_text('[]')
        self.manifest = dict(old_sha256=release.sha(self.base / release.BINARY),
                             new_sha256=release.sha(self.folder / 'workbuddy2api.next'),
                             config_sha256=release.sha(self.base / 'config.json'),
                             old_version='old', version='new', mode=0o750,
                             uid=os.getuid(), gid=os.getgid(), deploy_unit='test-deploy', recovery_unit='test-recovery')

    def test_atomic_install_preserves_old_open_executable(self):
        with (self.base / release.BINARY).open('rb') as old:
            release.install(self.folder / 'workbuddy2api.next', self.base / release.BINARY, 0o750, os.getuid(), os.getgid())
            self.assertEqual(old.read(), b'old binary')
        self.assertEqual((self.base / release.BINARY).read_bytes(), b'new binary')
        self.assertEqual((self.base / release.BINARY).stat().st_mode & 0o777, 0o750)

    def test_archive_backup_copies_and_validates_every_row(self):
        archive = self.base / 'data' / 'prompt-logs'
        archive.mkdir(parents=True)
        (archive / 'prompts-2026-10-19.jsonl').write_bytes(b'{"key_id":"a"}\n{"key_id":"b"}\n')
        result = release.archive_copy(self.base, self.folder / 'snapshot')
        self.assertEqual(result['rows'], 2)
        self.assertEqual((self.folder / 'snapshot' / 'prompts-2026-10-19.jsonl').read_bytes(), (archive / 'prompts-2026-10-19.jsonl').read_bytes())
        (archive / 'prompts-2026-10-19.jsonl').write_bytes(b'{broken\n')
        with self.assertRaises(json.JSONDecodeError):
            release.archive_copy(self.base, self.folder / 'damaged')

    def test_recovery_rejects_bad_backup_before_service_actions(self):
        (self.folder / 'started.json').write_text('{}')
        (self.folder / 'workbuddy2api.before').write_bytes(b'corrupt')
        with patch.object(release, 'run') as run, self.assertRaises(RuntimeError):
            release.recover(self.args, self.manifest)
        run.assert_not_called()

    def test_recovery_preserves_config_and_new_archive_data(self):
        (self.folder / 'started.json').write_text('{}')
        (self.base / release.BINARY).write_bytes(b'new binary')
        (self.base / 'config.json').write_text('{"new_operator_setting":true}')
        new_archive = self.base / 'new-input.jsonl'
        new_archive.write_text('new captures')
        with patch.object(release, 'run') as run, patch.object(release, 'wait_probe', return_value={'version':'old'}):
            release.recover(self.args, self.manifest)
        self.assertEqual((self.base / release.BINARY).read_bytes(), b'old binary')
        self.assertEqual(json.loads((self.base / 'config.json').read_text()), {'new_operator_setting':True})
        self.assertEqual(new_archive.read_text(), 'new captures')
        self.assertTrue((self.folder / 'recovered.json').exists())
        self.assertIn(('systemctl','restart',release.SERVICE), [c.args for c in run.call_args_list])
        self.assertEqual(run.call_args_list[-1].args, ('systemctl','stop','test-recovery.timer'))
        with patch.object(release, 'run') as second:
            release.recover(self.args, self.manifest)
        second.assert_not_called()

    def test_successful_activation_cannot_be_reverted_by_late_timer(self):
        (self.folder / 'success.json').write_text('{}')
        with patch.object(release, 'run') as run, patch.object(release, 'install') as install:
            release.recover(self.args, self.manifest)
        run.assert_not_called()
        install.assert_not_called()

    def test_failure_after_stop_recovers_without_overwriting_data(self):
        with patch.object(release, 'run', return_value='active') as run, \
             patch.object(release, 'archive_copy', side_effect=OSError('backup failure')), \
             patch.object(release, 'wait_probe', return_value={'version':'old'}):
            with self.assertRaises(OSError):
                release.activate(self.args, self.manifest)
        self.assertTrue((self.folder / 'recovered.json').exists())
        self.assertFalse((self.folder / 'success.json').exists())
        self.assertEqual((self.base / release.BINARY).read_bytes(), b'old binary')
        self.assertIn(('systemctl', 'restart', release.SERVICE), [c.args for c in run.call_args_list])

    def test_unarmed_activation_refuses_to_stop_service(self):
        with patch.object(release, 'run', return_value='inactive') as run, self.assertRaises(RuntimeError):
            release.activate(self.args, self.manifest)
        self.assertEqual(run.call_count, 1)
        self.assertEqual(run.call_args.args, ('systemctl','is-active','test-recovery.timer'))
        self.assertFalse((self.folder / 'started.json').exists())


if __name__ == '__main__':
    unittest.main()
