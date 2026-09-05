import importlib.machinery
import importlib.util
import os
import pathlib
import tempfile
import unittest
from unittest.mock import patch

PATH = pathlib.Path(__file__).with_name('dependency-client')
loader = importlib.machinery.SourceFileLoader('dependency_client', str(PATH))
spec = importlib.util.spec_from_loader(loader.name, loader)
m = importlib.util.module_from_spec(spec); loader.exec_module(m)

class DependencyClientTest(unittest.TestCase):
    def test_external_credentials_never_enter_command_arguments(self):
        values = {'DATABASE_HOST': 'db.internal', 'DATABASE_USER': 'user', 'DATABASE_PASSWORD': 'not-for-logs'}
        with patch.dict(os.environ, {'SUB2API_DEPENDENCY_MODE': 'external'}), patch.object(m, 'app_container', return_value=('sub2api-blue', values)):
            command = m.db_command('psql', ['-X', '-Atq', '--set=slot=blue'])
        self.assertNotIn('not-for-logs', ' '.join(command))
        self.assertIn('"$DATABASE_PASSWORD"', command[6])
        self.assertIn('--set=slot=blue', command)
        self.assertIn('${DATABASE_DBNAME:-${DATABASE_NAME:-sub2api}}', command[6])

    def test_missing_external_credentials_fail_closed(self):
        with patch.dict(os.environ, {'SUB2API_DEPENDENCY_MODE': 'external'}), patch.object(m, 'app_container', return_value=('sub2api-blue', {})):
            with self.assertRaises(RuntimeError): m.db_command('psql', [])

    def test_local_unhealthy_database_fails_closed(self):
        with patch.dict(os.environ, {'SUB2API_DEPENDENCY_MODE': 'local'}), patch.object(m, 'inspect', return_value={'State': {'Health': {'Status': 'unhealthy'}}}):
            with self.assertRaises(RuntimeError): m.db_command('pg_dump', ['-Fc'])

    def test_dump_does_not_consume_parent_bootstrap_stdin(self):
        import subprocess
        def dump(*args, **kwargs):
            self.assertIs(kwargs['stdin'], subprocess.DEVNULL)
            kwargs['stdout'].write(b'PGDMPverified-dump')
            return subprocess.CompletedProcess(args[0], 0)
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {'SUB2API_BACKUP_DIR': directory}), patch.object(m, 'db_command', return_value=['pg_dump']), patch.object(m.subprocess, 'run', side_effect=dump):
            target = pathlib.Path(directory) / 'new.dump'
            m.backup(str(target))
            self.assertEqual(b'PGDMPverified-dump', target.read_bytes())

    def test_backup_cannot_overwrite_or_escape_backup_directory(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {'SUB2API_BACKUP_DIR': directory}):
            target = pathlib.Path(directory) / 'existing.dump'; target.write_bytes(b'PGDMPoriginal')
            with self.assertRaises(RuntimeError): m.backup(str(target))
            with self.assertRaises(RuntimeError): m.backup(str(target.parent.parent / 'escape.dump'))
            self.assertEqual(b'PGDMPoriginal', target.read_bytes())

if __name__ == '__main__': unittest.main()
