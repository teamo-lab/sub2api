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
    def test_required_program_falls_back_to_healthy_container_with_tool(self):
        container = {
            'State': {'Running': True, 'Health': {'Status': 'healthy'}},
            'Config': {'Env': ['DATABASE_HOST=db.internal']},
        }
        containers = {
            'sub2api-worker': container,
            'sub2api-blue': container,
        }
        with patch.object(m, 'inspect', side_effect=lambda name: containers.get(name)), patch.object(
            m, 'container_has_program', side_effect=lambda name, program: name == 'sub2api-blue'
        ) as has_program:
            name, env = m.app_container('psql')
        self.assertEqual('sub2api-blue', name)
        self.assertEqual('db.internal', env['DATABASE_HOST'])
        self.assertEqual([('sub2api-worker', 'psql'), ('sub2api-blue', 'psql')], [call.args for call in has_program.call_args_list])

    def test_required_program_fails_when_no_healthy_container_has_tool(self):
        container = {'State': {'Running': True, 'Health': {'Status': 'healthy'}}, 'Config': {'Env': []}}
        with patch.object(m, 'inspect', return_value=container), patch.object(m, 'container_has_program', return_value=False):
            with self.assertRaisesRegex(RuntimeError, 'no healthy application container with pg_dump'):
                m.app_container('pg_dump')

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
