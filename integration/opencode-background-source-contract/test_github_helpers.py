#!/usr/bin/env python3
"""Offline helper regression tests; only synthetic credentials are used."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


SOURCE = Path(__file__).resolve().parents[2] / 'images/opencode-background-source'


class Helpers(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='fern-github-helpers-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.token = self.root / 'fern-github-token'
        self.repo = self.root / 'fern-github-repository'
        self.write(self.token, 'synthetic_token_one\n')
        self.write(self.repo, 'owner/repo\n')
        shared = self.root / 'credentials.cjs'
        shared.write_text((SOURCE / 'fern-github-credentials.cjs').read_text().replace(
            "'/home/user/.local/share/opencode'", json.dumps(str(self.root))))
        fake = self.root / 'real-gh'
        fake.write_text('#!' + shutil.which('node') + '\n' + """
console.log(JSON.stringify({args: process.argv.slice(2), env: process.env}));
process.exit(Number(process.env.FAKE_EXIT || 0));
""")
        fake.chmod(0o755)
        for name in ('gh', 'fern-git-credential'):
            text = (SOURCE / name).read_text().replace(
                '/usr/local/libexec/fern-github-credentials.cjs', str(shared)).replace(
                '/usr/local/libexec/gh', str(fake))
            (self.root / name).write_text(text)

    def write(self, path, value):
        path.write_text(value)
        path.chmod(0o600)

    def run_helper(self, name, *args, input='', **env):
        result = subprocess.run(
            [shutil.which('node'), str(self.root / name), *args], input=input,
            text=True, capture_output=True, env={**os.environ, **env})
        self.assertNotIn('synthetic_token', result.stderr)
        return result

    def credential(self, protocol='https', host='github.com', path='owner/repo', extra='', operation='get'):
        return self.run_helper('fern-git-credential', operation,
                               input=f'protocol={protocol}\nhost={host}\npath={path}\n{extra}\n')

    def test_exact_repository_and_rotation(self):
        for path in ('owner/repo', 'owner/repo.git'):
            self.assertEqual(self.credential(path=path).stdout,
                             'username=x-access-token\npassword=synthetic_token_one\n\n')
        self.write(self.token, 'synthetic_token_two\n')
        self.assertIn('password=synthetic_token_two', self.credential().stdout)

    def test_foreign_and_malformed_requests(self):
        for host in ('github.com:443', 'github.com.evil', 'user@github.com', 'GITHUB.COM', '', 'github.com/owner', 'github.com\r'):
            self.assertEqual(self.credential(host=host).stdout, '', host)
        for path in ('other/repo', 'owner/other', '/owner/repo', 'owner/repo/', 'owner/repo.git/extra',
                     'owner/../repo', 'owner%2frepo', 'owner/repo?x=1', 'owner/repo\x00', ''):
            self.assertEqual(self.credential(path=path).stdout, '', path)
        self.assertEqual(self.credential(protocol='http').stdout, '')
        self.assertEqual(self.credential(extra='host=github.com').stdout, '')
        self.assertEqual(self.credential(extra='malformed').stdout, '')
        for operation in ('store', 'erase', 'unknown'):
            self.assertEqual(self.credential(operation=operation).stdout, '')

    def test_invalid_files_fail_closed(self):
        for value in ('', 'bad\nsecond\n', 'bad\r\n', 'bad token'):
            self.write(self.token, value)
            self.assertEqual(self.credential().stdout, '')
            result = self.run_helper('gh', 'repo', 'view', GH_TOKEN='synthetic_token_inherited')
            self.assertEqual(result.returncode, 1)
            self.assertEqual(result.stdout, '')
        self.write(self.token, 'synthetic_token_one')
        self.token.chmod(0o644)
        self.assertEqual(self.credential().stdout, '')
        self.token.unlink()
        self.assertEqual(self.credential().stdout, '')
        self.assertEqual(self.run_helper('gh', 'repo', 'view').returncode, 1)
        self.assertEqual(self.run_helper('gh', '--version').returncode, 0)
        self.token.symlink_to(self.repo)
        self.assertEqual(self.credential().stdout, '')

    def test_invalid_repository(self):
        for value in ('', 'owner/repo\nother/repo\n', '../repo', 'owner/..', 'owner/repo/extra'):
            self.write(self.repo, value)
            self.assertEqual(self.credential().stdout, '')
        self.repo.unlink()
        self.assertEqual(self.credential().stdout, '')

    def test_gh_arguments_environment_exit_and_cleanup(self):
        result = self.run_helper('gh', 'pr', 'view', 'argument with spaces',
                                 GH_TOKEN='synthetic_token_inherited', GITHUB_TOKEN='personal',
                                 GH_ENTERPRISE_TOKEN='personal', GITHUB_ENTERPRISE_TOKEN='personal',
                                 GH_CONFIG_DIR='/personal/config', GH_DEBUG='api', GH_HOST='foreign',
                                 FAKE_EXIT='7')
        self.assertEqual(result.returncode, 7)
        payload = json.loads(result.stdout)
        self.assertEqual(payload['args'], ['pr', 'view', 'argument with spaces'])
        env = payload['env']
        self.assertEqual(env['GH_TOKEN'], 'synthetic_token_one')
        self.assertEqual(env['GH_HOST'], 'github.com')
        self.assertEqual(env['GH_PROMPT_DISABLED'], '1')
        for key in ('GITHUB_TOKEN', 'GH_ENTERPRISE_TOKEN', 'GITHUB_ENTERPRISE_TOKEN', 'GH_DEBUG'):
            self.assertNotIn(key, env)
        self.assertTrue(env['GH_CONFIG_DIR'].startswith(str(self.root) + '/fern-gh-config-'))
        self.assertFalse(Path(env['GH_CONFIG_DIR']).exists())
        self.write(self.token, 'synthetic_token_two')
        self.assertEqual(json.loads(self.run_helper('gh', 'repo', 'view').stdout)['env']['GH_TOKEN'],
                         'synthetic_token_two')

    def test_auth_commands_blocked(self):
        for args in (('auth', 'login'), ('auth', 'token'), ('--repo', 'owner/repo', 'auth', 'login')):
            result = self.run_helper('gh', *args)
            self.assertEqual(result.returncode, 1)
            self.assertEqual(result.stdout, '')

    def test_auth_in_command_arguments_is_not_a_subcommand(self):
        result = self.run_helper('gh', 'pr', 'create', '--title', 'auth')
        self.assertEqual(result.returncode, 0)


if __name__ == '__main__':
    unittest.main()
