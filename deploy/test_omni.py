"""Offline regression checks for release identity, SSH boundaries and rollback."""

import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location("omni_deployment", Path(__file__).with_name("omni.py"))
omni = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(omni)

REVISION = "a" * 40
ARTIFACT_DIGEST = "sha256:" + "b" * 64
API_URL = "https://node.example.test:8080/api/system"


class OfflineTest(unittest.TestCase):
    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.directory = Path(self.stack.enter_context(tempfile.TemporaryDirectory(prefix="omni-unit-", dir=Path(__file__).resolve().parent)))
        previous = Path.cwd()
        os.chdir(self.directory)
        self.stack.callback(os.chdir, previous)
        self.stack.enter_context(patch.object(tempfile, "tempdir", str(self.directory)))
        self.stack.enter_context(patch.dict(os.environ, {
            "GITHUB_REF": "refs/tags/v1.2.3", "GITHUB_SHA": REVISION,
            "GITHUB_RUN_ID": "123", "GITHUB_OUTPUT": str(self.directory / "output"),
            "SSH_PRIVATE_KEY": "synthetic-unit-test-key", "SSH_KNOWN_HOSTS": "synthetic-unit-test-host-key",
        }, clear=True))
        self.output = io.StringIO()
        self.stack.enter_context(contextlib.redirect_stdout(self.output))
        self.github = self.stack.enter_context(patch.object(omni, "github", side_effect=AssertionError("Unexpected GitHub call")))
        self.run = self.stack.enter_context(patch.object(omni, "run", side_effect=AssertionError("Unexpected command")))
        self.raw_process = self.stack.enter_context(patch.object(omni.subprocess, "run", side_effect=AssertionError("Real subprocess forbidden")))

    def environment(self, name="omni-production", reviewers=True):
        return {
            "name": name,
            "deployment_branch_policy": {"custom_branch_policies": True, "protected_branches": False},
            "protection_rules": [{"type": "required_reviewers", "reviewers": [{"type": "User", "reviewer": {"id": 1}}]}] if reviewers else [],
        }

    def metadata(self):
        return {arch: {"id": index + 1, "digest": ARTIFACT_DIGEST} for index, arch in enumerate(omni.ARCHES)}

    def artifacts(self):
        return [{"name": f"omni_linux_{arch}", "id": index + 1, "digest": ARTIFACT_DIGEST, "expired": False}
                for index, arch in enumerate(omni.ARCHES)]

    def bundle_files(self):
        os.environ["OMNI_ARTIFACTS"] = json.dumps(self.metadata())
        for arch in omni.ARCHES:
            directory = self.directory / "artifacts" / f"omni_linux_{arch}"
            directory.mkdir(parents=True, exist_ok=True)
            binary = directory / "omni"
            binary.write_bytes(f"synthetic {arch} executable".encode())
            (directory / "SHA256SUMS").write_text(f"{omni.digest(binary)}  omni\n")
            (directory / "REVISION").write_text(REVISION + "\n")
        self.github.side_effect = lambda path: next(
            artifact for artifact in self.artifacts() if str(artifact["id"]) == path.rsplit("/", 1)[-1])


class ReleasePolicyTest(OfflineTest):
    def test_required_settings_reject_missing_and_whitespace(self):
        for value in (None, "", " \t\n"):
            with self.subTest(value=value):
                if value is None:
                    os.environ.pop("SETTING", None)
                else:
                    os.environ["SETTING"] = value
                with self.assertRaisesRegex(ValueError, "Missing required setting: SETTING"):
                    omni.required("SETTING")
        os.environ["SETTING"] = " value "
        self.assertEqual("value", omni.required("SETTING"))

    def test_environment_must_exist_and_use_only_selected_release_tags(self):
        for update in ({"name": "other"}, {"deployment_branch_policy": None},
                       {"deployment_branch_policy": {"custom_branch_policies": False, "protected_branches": True}}):
            with self.subTest(update=update):
                self.github.side_effect = None
                self.github.return_value = dict(self.environment(), **update)
                with self.assertRaises(ValueError):
                    omni.check_environment("omni-production")
        self.github.side_effect = OSError("environment unavailable")
        with self.assertRaises(OSError):
            omni.check_environment("omni-production")

    def test_production_requires_manual_reviewers(self):
        self.github.side_effect = [self.environment(reviewers=False), {"branch_policies": [{"type": "tag", "name": "v*"}]}]
        with self.assertRaisesRegex(ValueError, "manual reviewers"):
            omni.check_environment("omni-production")

    def test_required_reviewers_accept_only_explicit_user_or_team_identities(self):
        for kind in ("User", "Team"):
            environment = self.environment()
            environment["protection_rules"][0]["reviewers"] = [{"type": kind, "reviewer": {"id": 7}}]
            self.github.side_effect = [environment, {"branch_policies": [{"type": "tag", "name": "v*"}]}]
            omni.check_environment("omni-production")

    def test_malformed_reviewer_policies_cannot_unlock_production(self):
        invalid = (True, {}, "reviewer", [], None, [{}], [True],
                   [{"type": "User", "reviewer": {}}], [{"type": "User", "reviewer": {"id": 0}}],
                   [{"type": "User", "reviewer": {"id": True}}], [{"type": "User", "reviewer": {"id": "1"}}],
                   [{"type": "Bot", "reviewer": {"id": 1}}],
                   [{"type": "User", "reviewer": {"id": 1}}, {"type": "User", "reviewer": {}}])
        for reviewers in invalid:
            with self.subTest(reviewers=reviewers):
                environment = self.environment()
                environment["protection_rules"][0]["reviewers"] = reviewers
                self.github.side_effect = [environment, {"branch_policies": [{"type": "tag", "name": "v*"}]}]
                with self.assertRaises(ValueError):
                    omni.check_environment("omni-production")

    def test_pagination_checks_up_to_ten_pages_and_rejects_possible_overflow(self):
        full_page = {"items": [{"id": 1}] * 100}
        self.github.side_effect = [full_page] * 9 + [{"items": [{"id": 2}]}]
        self.assertEqual(901, len(list(omni.pages("synthetic", "items"))))
        self.assertEqual(10, self.github.call_count)
        self.github.reset_mock()
        self.github.side_effect = [full_page] * 10 + [AssertionError("Pagination exceeded ten-page cap")]
        with self.assertRaises(ValueError):
            list(omni.pages("synthetic", "items"))
        self.assertLessEqual(self.github.call_count, 10)

    def test_pagination_rejects_invalid_response_containers_and_nonobject_records(self):
        for response in (None, [], {}, {"items": {}}, {"items": [None]}, {"items": ["record"]},
                         {"items": [True]}, {"items": [[]]}, {"items": [{}] * 101}):
            with self.subTest(response=response):
                self.github.side_effect = [response]
                with self.assertRaises(ValueError):
                    list(omni.pages("synthetic", "items"))

    def test_tag_policy_is_checked_on_every_page(self):
        self.github.side_effect = [self.environment(), {"branch_policies": [{"type": "tag", "name": "v*"}] * 100},
                                   {"branch_policies": [{"type": "branch", "name": "develop"}]}]
        with self.assertRaisesRegex(ValueError, "only the v\\* tag policy"):
            omni.check_environment("omni-production")
        self.assertIn("page=2", self.github.call_args.args[0])

    def test_empty_or_broad_tag_policies_are_rejected(self):
        for rules in ([], [{"type": "tag", "name": "*"}], [{"type": "branch", "name": "v*"}]):
            with self.subTest(rules=rules):
                self.github.side_effect = [self.environment(), {"branch_policies": rules}]
                with self.assertRaises(ValueError):
                    omni.check_environment("omni-production")

    def test_prepare_rejects_nonrelease_refs_before_other_actions(self):
        for ref in ("refs/heads/develop", "refs/tags/latest", "refs/tags/v1.2", "refs/tags/v1.2.3\nunsafe"):
            with self.subTest(ref=ref), patch.dict(os.environ, {"GITHUB_REF": ref}):
                with self.assertRaises(ValueError):
                    omni.prepare()
        self.github.assert_not_called()
        self.assertFalse((self.directory / "output").exists())

    def test_prepare_outputs_only_both_verified_artifact_identities(self):
        self.github.side_effect = [{"artifacts": self.artifacts()}]
        os.environ["GITHUB_STEP_SUMMARY"] = str(self.directory / "summary")
        with patch.object(omni, "check_environment") as policy:
            omni.prepare()
        self.assertEqual([("omni-canary",), ("omni-production",)], [call.args for call in policy.call_args_list])
        output = (self.directory / "output").read_text()
        self.assertIn("artifact_ids=1,2\n", output)
        identities = json.loads(next(line.partition("=")[2] for line in output.splitlines() if line.startswith("artifacts=")))
        self.assertEqual(self.metadata(), identities)
        summary = (self.directory / "summary").read_text()
        self.assertIn(REVISION, summary)
        for arch, artifact in self.metadata().items():
            self.assertIn(f"{arch}: artifact `{artifact['id']}`, digest `{artifact['digest']}`", summary)

    def test_prepare_rejects_missing_expired_duplicate_and_unverifiable_artifacts(self):
        base = self.artifacts()
        cases = [base[:1], base + [base[0]], [dict(base[0], expired=True), base[1]],
                 [dict(base[0], digest=""), base[1]], [dict(base[0], expired=None), base[1]]]
        for artifacts in cases:
            with self.subTest(artifacts=artifacts), patch.object(omni, "check_environment"):
                self.github.side_effect = [{"artifacts": artifacts}]
                with self.assertRaises(ValueError):
                    omni.prepare()
        self.assertFalse((self.directory / "output").exists())

    def test_prepare_rejects_nonpositive_noninteger_and_boolean_artifact_ids(self):
        for identity in (0, -1, None, "1", True):
            with self.subTest(identity=identity), patch.object(omni, "check_environment"):
                self.github.side_effect = [{"artifacts": [dict(self.artifacts()[0], id=identity), self.artifacts()[1]]}]
                with self.assertRaises(ValueError):
                    omni.prepare()


class BundleAndNodeTest(OfflineTest):
    def test_node_requires_exact_fields_valid_ipv4_and_safe_ssh_user(self):
        valid = {"host": "198.51.100.20", "user": "deploy_user", "api_url": API_URL}
        self.assertEqual(valid, omni.node(valid))
        cases = [dict(valid, host="127.0.0.1"), dict(valid, host="0.0.0.0"), dict(valid, host="224.0.0.1"),
                 dict(valid, host=3325256724), dict(valid, host=True), dict(valid, host=None),
                 dict(valid, host="198.051.100.20"), dict(valid, host=" 198.51.100.20"),
                 dict(valid, host="node.example.test"), dict(valid, host="198.51.100.20; touch bad"),
                 dict(valid, user="root;bad"), dict(valid, user="-root"), dict(valid, unexpected="field"),
                 {"host": valid["host"], "user": "root"}, []]
        for value in cases:
            with self.subTest(value=value), self.assertRaises(ValueError):
                omni.node(value)

    def test_health_url_cannot_carry_credentials_query_fragment_or_commands(self):
        self.assertEqual(("node.example.test", 8080), omni.health_address(API_URL))
        self.assertEqual(("node.example.test", 443), omni.health_address("https://node.example.test/api/system"))
        for url in ("https://user:secret@node.example.test/api/system", API_URL + "?token=x", API_URL + "#fragment",
                    "https://node.example.test/api/system;touch", "file:///api/system", "https://node.example.test/health",
                    "http://node.example.test:0/api/system", "http://node.example.test:65536/api/system"):
            with self.subTest(url=url), self.assertRaises(ValueError):
                omni.health_address(url)

    def test_health_url_must_be_a_string(self):
        for value in (None, True, 123, [], {}, b"http://node.example.test/api/system"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                omni.health_address(value)

    def test_valid_bundles_match_checksum_revision_and_artifact_identity(self):
        self.bundle_files()
        result = omni.bundles()
        self.assertEqual(set(omni.ARCHES), set(result))
        for path, checksum in result.values():
            self.assertEqual(omni.digest(path), checksum)

    def test_changed_artifact_identity_is_rejected(self):
        self.bundle_files()
        for changes in ({"digest": "sha256:" + "c" * 64}, {"expired": True}, {"name": "unrelated"}):
            with self.subTest(changes=changes):
                self.github.side_effect = None
                self.github.return_value = dict(self.artifacts()[0], **changes)
                with self.assertRaisesRegex(ValueError, "identity changed"):
                    omni.bundles()

    def test_missing_bundle_members_are_rejected(self):
        for name in ("omni", "SHA256SUMS", "REVISION"):
            with self.subTest(name=name):
                self.bundle_files()
                (self.directory / "artifacts/omni_linux_amd64" / name).unlink()
                with self.assertRaisesRegex(ValueError, "Incomplete artifact bundle"):
                    omni.bundles()

    def test_changed_binary_checksum_revision_or_checksum_filename_is_rejected(self):
        for filename, value in (("omni", "changed binary"), ("REVISION", "c" * 40),
                                ("SHA256SUMS", "d" * 64 + "  other-binary\n")):
            with self.subTest(filename=filename):
                self.bundle_files()
                (self.directory / "artifacts/omni_linux_amd64" / filename).write_text(value)
                with self.assertRaisesRegex(ValueError, "checksum or revision"):
                    omni.bundles()

    @unittest.skipUnless(hasattr(os, "symlink") and os.name != "nt", "Symlink fixture requires Unix")
    def test_symlink_bundle_members_are_rejected(self):
        self.bundle_files()
        binary = self.directory / "artifacts/omni_linux_amd64/omni"
        real = self.directory / "elsewhere"
        binary.rename(real)
        binary.symlink_to(real)
        with self.assertRaisesRegex(ValueError, "Incomplete artifact bundle"):
            omni.bundles()

    def test_bundle_artifact_ids_must_be_positive_real_integers(self):
        self.bundle_files()
        for identity in (0, -1, "1", None, True):
            with self.subTest(identity=identity):
                metadata = self.metadata()
                metadata["amd64"]["id"] = identity
                os.environ["OMNI_ARTIFACTS"] = json.dumps(metadata)
                with self.assertRaisesRegex(ValueError, "Invalid artifact identity"):
                    omni.bundles()


class ProbeAndGameStateTest(OfflineTest):
    def test_probe_needs_expected_401_headers_and_pins_connection_to_loopback(self):
        self.run.side_effect = None
        self.run.return_value = "HTTP/1.1 401 Unauthorized\r\nWWW-Authenticate: Bearer\r\nX-Request-Id: synthetic-id\r\n\r\n\n401"
        self.assertTrue(omni.probe(API_URL))
        arguments = self.run.call_args.args[0]
        self.assertEqual("node.example.test:8080:127.0.0.1", arguments[arguments.index("--resolve") + 1])
        self.assertEqual("*", arguments[arguments.index("--noproxy") + 1])
        self.assertNotIn("--insecure", arguments)
        self.assertNotIn("-k", arguments)
        for response in ("HTTP/1.1 200 OK\r\n\n200", "HTTP/1.1 401 Unauthorized\r\n\n401",
                         "WWW-Authenticate: Bearer\r\n\n401", "X-Request-Id: synthetic-id\r\n\n401"):
            with self.subTest(response=response):
                self.run.return_value = response
                self.assertFalse(omni.probe(API_URL))

    def test_game_baseline_reads_only_running_game_containers_and_safe_inspect_fields(self):
        identity = "a" * 64
        self.run.side_effect = [identity, f"{identity} true 2026-10-30T10:00:00Z 0"]
        self.assertEqual({identity: ["true", "2026-10-30T10:00:00Z", "0"]}, omni.game_state())
        listing, inspect = (call.args[0] for call in self.run.call_args_list)
        self.assertIn("label=Service=Pterodactyl", listing)
        self.assertIn("label=ContainerType=server_process", listing)
        self.assertIn("status=running", listing)
        self.assertEqual(["docker", "inspect", "--format", "{{.Id}} {{.State.Running}} {{.State.StartedAt}} {{.RestartCount}}", identity], inspect)
        self.run.reset_mock()
        with self.assertRaisesRegex(ValueError, "container identity"):
            omni.game_state(["invalid; command"])
        self.run.assert_not_called()

    def test_game_state_rejects_missing_duplicate_nonrunning_and_malformed_rows(self):
        identity = "a" * 64
        valid = f"{identity} true 2026-10-30T10:00:00Z 0"
        for rows in ("", valid + "\n" + valid, valid.replace(" true ", " false "),
                     f"{identity} true", valid.replace(identity, "b" * 64)):
            with self.subTest(rows=rows):
                self.run.side_effect = None
                self.run.return_value = rows
                with self.assertRaises(ValueError):
                    omni.game_state([identity])


class RolloutTest(OfflineTest):
    def setUp(self):
        super().setUp()
        self.policy = self.stack.enter_context(patch.object(omni, "check_environment"))
        self.bundle = self.stack.enter_context(patch.object(omni, "bundles", return_value={"amd64": (Path("fake-omni"), "a" * 64)}))

    def inventory(self, *hosts):
        os.environ["OMNI_DEPLOY_NODES"] = json.dumps([{"host": host, "user": "minat", "api_url": API_URL} for host in hosts])

    def remote(self, arguments, timeout=60):
        self.commands.append(arguments)
        if arguments[0] == "scp":
            return ""
        self.ssh_configs.append(Path(arguments[2]).read_text())
        command = arguments[-1]
        if command == "uname -m":
            return "x86_64"
        if command.startswith("mktemp "):
            return "/tmp/omni-ci.ABC123xy"
        if "python3 " in command:
            return "verified synthetic install"
        if command.startswith("rm "):
            return ""
        raise AssertionError("Unexpected mocked SSH operation")

    def test_empty_nonlist_duplicate_or_canary_only_inventory_is_rejected(self):
        cases = [[], {}, [{"host": omni.CANARY, "user": "root", "api_url": API_URL}],
                 [{"host": "198.51.100.20", "user": "root", "api_url": API_URL}] * 2]
        for values in cases:
            with self.subTest(values=values), patch.dict(os.environ, {"OMNI_DEPLOY_NODES": json.dumps(values)}):
                with self.assertRaises(ValueError):
                    omni.rollout("fleet")
        self.run.assert_not_called()

    def test_fleet_skips_canary_and_uses_strict_ssh_jump_and_passwordless_sudo(self):
        self.inventory(omni.CANARY, "5.166.219.51")
        self.commands, self.ssh_configs = [], []
        self.run.side_effect = self.remote
        omni.rollout("fleet")
        self.policy.assert_called_once_with("omni-production")
        self.assertEqual(1, sum(command[-1] == "uname -m" for command in self.commands))
        config = self.ssh_configs[0]
        for setting in ("Port 2222", "BatchMode yes", "IdentitiesOnly yes", "StrictHostKeyChecking yes", "ProxyJump omni-jump"):
            self.assertIn(setting, config)
        self.assertIn("sudo -n python3 ", next(command[-1] for command in self.commands if "python3 " in command[-1]))
        self.assertFalse(any("docker" in command for command in self.commands))

    def test_unknown_architecture_fails_before_staging_or_copy(self):
        self.inventory("198.51.100.20")
        self.run.side_effect = None
        self.run.return_value = "riscv64"
        with self.assertRaisesRegex(ValueError, "Unsupported node architecture"):
            omni.rollout("fleet")
        self.assertEqual(1, self.run.call_count)

    def test_first_failed_node_stops_fleet_and_still_cleans_its_staging_directory(self):
        self.inventory("198.51.100.20", "198.51.100.21")
        self.commands, self.ssh_configs = [], []

        def fail_install(arguments, timeout=60):
            if "python3 " in arguments[-1]:
                self.commands.append(arguments)
                raise subprocess.CalledProcessError(1, ["synthetic-install"])
            return self.remote(arguments, timeout)

        self.run.side_effect = fail_install
        with self.assertRaises(subprocess.CalledProcessError):
            omni.rollout("fleet")
        self.assertEqual(1, sum(command[-1] == "uname -m" for command in self.commands))
        self.assertTrue(self.commands[-1][-1].startswith("rm -r -- /tmp/omni-ci."))
        self.assertFalse(any(command[3] == "omni-1" for command in self.commands if command[0] == "ssh"))

    def test_remote_diagnostics_are_prefixed_and_cleanup_cannot_mask_install_failure(self):
        self.inventory("198.51.100.20", "198.51.100.21")
        self.commands, self.ssh_configs = [], []
        backup_path = "/usr/local/bin/.omni-backup-synthetic"
        original = subprocess.CalledProcessError(17, ["synthetic-install"], output=b"install output\x00\n",
                                                stderr=f"rollback failed at {backup_path}\n::warning::synthetic annotation\x1b[31m")

        def fail_install_and_cleanup(arguments, timeout=60):
            if "python3 " in arguments[-1]:
                self.commands.append(arguments)
                raise original
            if arguments[-1].startswith("rm "):
                self.commands.append(arguments)
                raise subprocess.CalledProcessError(19, ["synthetic-cleanup"])
            return self.remote(arguments, timeout)

        self.run.side_effect = fail_install_and_cleanup
        errors = io.StringIO()
        with contextlib.redirect_stderr(errors), self.assertRaises(subprocess.CalledProcessError) as raised:
            omni.rollout("fleet")
        self.assertIs(original, raised.exception)
        diagnostics = errors.getvalue()
        self.assertIn(f"Remote diagnostic: rollback failed at {backup_path}", diagnostics)
        self.assertIn("Remote diagnostic: install output", diagnostics)
        self.assertIn("Remote diagnostic: ::warning::synthetic annotation", diagnostics)
        self.assertNotIn("\x00", diagnostics)
        self.assertNotIn("\x1b", diagnostics)
        self.assertFalse(any(line.startswith("::") for line in diagnostics.splitlines()))
        self.assertIn("preserving the original deployment error", diagnostics)
        self.assertEqual(1, sum(command[-1] == "uname -m" for command in self.commands))

    def test_unexpected_remote_staging_path_cannot_reach_scp_or_cleanup(self):
        self.inventory("198.51.100.20")
        self.run.side_effect = ["x86_64", "/tmp/omni-ci.safe; unsafe"]
        with self.assertRaisesRegex(ValueError, "Unexpected remote staging path"):
            omni.rollout("fleet")
        self.assertEqual(2, self.run.call_count)


@unittest.skipUnless(sys.platform.startswith("linux"), "Installer ownership and atomic replacement fixtures require Linux")
class InstallTest(OfflineTest):
    def setUp(self):
        super().setUp()
        if os.geteuid() != 0:
            self.skipTest("Root-owned binary fixtures run in the isolated Linux test container")
        self.binary = self.directory / "bin/omni"
        self.binary.parent.mkdir()
        self.binary.write_bytes(b"old executable")
        self.binary.chmod(0o751)
        self.artifact = self.directory / "candidate"
        self.artifact.write_bytes(b"new executable")
        self.old_checksum, self.new_checksum = omni.digest(self.binary), omni.digest(self.artifact)
        self.stack.enter_context(patch.object(omni, "BINARY", self.binary))
        self.stack.enter_context(patch.object(omni.os, "geteuid", return_value=0))
        real_digest = omni.digest
        self.stack.enter_context(patch.object(omni, "digest", side_effect=lambda path: self.old_checksum if str(path).startswith("/proc/") else real_digest(path)))
        self.stack.enter_context(patch.object(omni, "install_lock", side_effect=contextlib.nullcontext))
        self.probe = self.stack.enter_context(patch.object(omni, "probe", return_value=True))
        self.ready = self.stack.enter_context(patch.object(omni, "wait_ready"))
        self.baseline = {"a" * 64: ["true", "2026-10-30T10:00:00Z", "0"]}
        self.games = self.stack.enter_context(patch.object(omni, "game_state", return_value=self.baseline))
        self.stack.enter_context(patch.object(omni.signal, "signal", return_value=signal.SIG_DFL))
        self.run.side_effect = lambda arguments, timeout=60: "123" if "show" in arguments else ""

    def assert_no_docker_restart(self):
        for call in self.run.call_args_list:
            arguments = call.args[0]
            self.assertFalse(arguments[:2] in (["docker", "restart"], ["docker", "stop"], ["docker", "kill"]))
            if arguments[:2] == ["systemctl", "restart"]:
                self.assertEqual([omni.SERVICE], arguments[2:])

    def test_checksum_mismatch_does_not_change_binary_or_restart(self):
        with self.assertRaisesRegex(ValueError, "checksum mismatch"):
            omni.install(self.artifact, "0" * 64, API_URL)
        self.assertEqual(b"old executable", self.binary.read_bytes())
        self.run.assert_not_called()
        self.probe.assert_not_called()

    def test_success_keeps_original_backup_and_file_mode(self):
        omni.install(self.artifact, self.new_checksum, API_URL)
        self.assertEqual(b"new executable", self.binary.read_bytes())
        self.assertEqual(b"old executable", self.binary.with_name("omni.previous").read_bytes())
        self.assertEqual(0o751, stat.S_IMODE(self.binary.stat().st_mode))
        self.assertEqual(0o751, stat.S_IMODE(self.binary.with_name("omni.previous").stat().st_mode))
        self.ready.assert_called_once_with(self.new_checksum, API_URL)
        self.assertEqual([], list(self.binary.parent.glob(".omni-*")))
        self.assert_no_docker_restart()

    def test_readiness_failure_restores_old_binary_and_restarts_old_agent(self):
        self.ready.side_effect = [RuntimeError("candidate not ready"), None]
        with self.assertRaisesRegex(RuntimeError, "candidate not ready"):
            omni.install(self.artifact, self.new_checksum, API_URL)
        self.assertEqual(b"old executable", self.binary.read_bytes())
        self.assertEqual([self.new_checksum, self.old_checksum], [call.args[0] for call in self.ready.call_args_list])
        self.assertEqual(2, sum(call.args[0] == ["systemctl", "restart", omni.SERVICE] for call in self.run.call_args_list))
        self.assertEqual([], list(self.binary.parent.glob(".omni-*")))
        self.assert_no_docker_restart()

    def test_failed_rollback_keeps_the_old_copy_and_reports_its_path(self):
        real_replace = omni.os.replace

        def fail_restoration(source, destination):
            if Path(source).name.startswith(".omni-backup-") and Path(destination) == self.binary:
                raise OSError("synthetic rollback rename failure")
            return real_replace(source, destination)

        self.ready.side_effect = RuntimeError("candidate not ready")
        errors = io.StringIO()
        with patch.object(omni.os, "replace", side_effect=fail_restoration), contextlib.redirect_stderr(errors):
            with self.assertRaises((OSError, RuntimeError)) as raised:
                omni.install(self.artifact, self.new_checksum, API_URL)
        backups = list(self.binary.parent.glob(".omni-backup-*"))
        self.assertEqual(1, len(backups), "The only recoverable old binary must survive failed rollback")
        self.assertEqual(b"old executable", backups[0].read_bytes())
        self.assertIn(str(backups[0]), str(raised.exception) + self.output.getvalue() + errors.getvalue())
        self.assert_no_docker_restart()

    def test_interruption_after_atomic_swap_still_restores_the_original_binary(self):
        changes = 0

        @contextlib.contextmanager
        def interrupt_on_first_unblock():
            nonlocal changes
            changes += 1
            yield
            if changes == 1:
                raise InterruptedError("synthetic pending termination signal")

        with patch.object(omni, "atomic_changes", side_effect=interrupt_on_first_unblock):
            with self.assertRaisesRegex(InterruptedError, "pending termination signal"):
                omni.install(self.artifact, self.new_checksum, API_URL)
        self.assertEqual(b"old executable", self.binary.read_bytes())
        self.ready.assert_called_once_with(self.old_checksum, API_URL)
        self.assertEqual([], list(self.binary.parent.glob(".omni-*")))
        self.assert_no_docker_restart()

    def test_changed_baseline_game_container_rolls_back(self):
        self.games.side_effect = [self.baseline, {"a" * 64: ["true", "2026-10-30T10:01:00Z", "1"]}]
        with self.assertRaisesRegex(RuntimeError, "baseline game container changed"):
            omni.install(self.artifact, self.new_checksum, API_URL)
        self.assertEqual(b"old executable", self.binary.read_bytes())
        self.assertEqual(self.old_checksum, self.ready.call_args.args[0])
        self.assert_no_docker_restart()

    def test_same_verified_binary_needs_no_restart_or_backup(self):
        self.artifact.write_bytes(self.binary.read_bytes())
        omni.install(self.artifact, self.old_checksum, API_URL)
        self.ready.assert_not_called()
        self.games.assert_not_called()
        self.assertFalse(self.binary.with_name("omni.previous").exists())
        self.assertFalse(any(call.args[0][:2] == ["systemctl", "restart"] for call in self.run.call_args_list))


if __name__ == "__main__":
    unittest.main()
