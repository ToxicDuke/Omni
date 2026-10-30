#!/usr/bin/env python3
"""Omni's release-only, environment-approved binary rollout."""

import argparse
import contextlib
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time
from urllib.parse import urlsplit
from urllib.request import Request, urlopen


CANARY = "31.130.204.250"
BINARY = Path("/usr/local/bin/omni")
SERVICE = "omni.service"
ARCHES = ("amd64", "arm64")


def required(name):
    value = os.environ.get(name, "").strip()
    if not value:
        raise ValueError(f"Missing required setting: {name}")
    return value


def github(path):
    repository = required("GITHUB_REPOSITORY")
    if not re.fullmatch(r"[\w.-]+/[\w.-]+", repository):
        raise ValueError("Invalid repository identity")
    request = Request(f"https://api.github.com/repos/{repository}/{path}", headers={
        "Authorization": f"Bearer {required('GH_TOKEN')}",
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
    })
    with urlopen(request, timeout=15) as response:
        return json.load(response)


def pages(path, key):
    # shortcut: reject over 1,000 records; review the bound before supporting larger release runs.
    for page in range(1, 11):
        response = github(f"{path}?per_page=100&page={page}")
        values = response.get(key) if isinstance(response, dict) else None
        if not isinstance(values, list) or len(values) > 100 or any(not isinstance(value, dict) for value in values):
            raise ValueError("Invalid GitHub response")
        yield from values
        if len(values) < 100:
            return
    raise ValueError("GitHub pagination exceeded the reviewed bound")


def check_environment(name):
    environment = github(f"environments/{name}")
    if not isinstance(environment, dict):
        raise ValueError("Invalid GitHub environment response")
    policy = environment.get("deployment_branch_policy") or {}
    if environment.get("name") != name or policy.get("custom_branch_policies") is not True or policy.get("protected_branches") is not False:
        raise ValueError(f"{name} must already exist with selected tag policies")
    rules = list(pages(f"environments/{name}/deployment-branch-policies", "branch_policies"))
    if not rules or any(rule.get("type") != "tag" or rule.get("name") != "v*" for rule in rules):
        raise ValueError(f"{name} must allow only the v* tag policy")
    if name == "omni-production":
        for rule in environment.get("protection_rules", []):
            reviewers = rule.get("reviewers")
            if rule.get("type") == "required_reviewers" and isinstance(reviewers, list) and reviewers and all(
                isinstance(value, dict) and value.get("type") in ("User", "Team")
                and isinstance(value.get("reviewer"), dict)
                and type(value["reviewer"].get("id")) is int and value["reviewer"]["id"] > 0
                for value in reviewers
            ):
                return
        raise ValueError("omni-production requires manual reviewers; refusing automatic fleet deployment")


def prepare():
    if not re.fullmatch(r"refs/tags/v\d+\.\d+\.\d+(?:[+.-][A-Za-z0-9.-]+)?", required("GITHUB_REF")):
        raise ValueError("Only versioned release tags can deploy")
    if not re.fullmatch(r"[a-f0-9]{40}", required("GITHUB_SHA")):
        raise ValueError("Invalid release revision")
    for name in ("omni-canary", "omni-production"):
        check_environment(name)
    run = required("GITHUB_RUN_ID")
    if not run.isdigit():
        raise ValueError("Invalid workflow run")
    artifacts = {}
    for artifact in pages(f"actions/runs/{run}/artifacts", "artifacts"):
        for arch in ARCHES:
            if artifact.get("name") == f"omni_linux_{arch}":
                if arch in artifacts or type(artifact.get("id")) is not int or artifact["id"] <= 0 or artifact.get("expired") is not False or not re.fullmatch(r"sha256:[a-f0-9]{64}", artifact.get("digest", "")):
                    raise ValueError("Missing, expired, duplicate or unverifiable build artifact")
                artifacts[arch] = {"id": artifact["id"], "digest": artifact["digest"]}
    if set(artifacts) != set(ARCHES):
        raise ValueError("Both tested Linux artifacts are required")
    with open(required("GITHUB_OUTPUT"), "a", encoding="utf-8") as output:
        output.write("canary_environment=omni-canary\nproduction_environment=omni-production\n")
        output.write("artifact_ids=" + ",".join(str(artifacts[arch]["id"]) for arch in ARCHES) + "\n")
        output.write("artifacts=" + json.dumps(artifacts, separators=(",", ":")) + "\n")
    if os.environ.get("GITHUB_STEP_SUMMARY"):
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a", encoding="utf-8") as summary:
            summary.write(f"## Omni release candidate\n\nRevision: `{required('GITHUB_SHA')}`\n\n")
            for arch in ARCHES:
                summary.write(f"- {arch}: artifact `{artifacts[arch]['id']}`, digest `{artifacts[arch]['digest']}`\n")


def digest(path):
    value = hashlib.sha256()
    with open(path, "rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def health_address(url):
    if not isinstance(url, str) or not re.fullmatch(r"https?://[A-Za-z0-9.-]+(?::[0-9]{1,5})?/api/system", url):
        raise ValueError("api_url must be an HTTP(S) /api/system URL without credentials or query parameters")
    parsed = urlsplit(url)
    port = parsed.port if parsed.port is not None else (443 if parsed.scheme == "https" else 80)
    if not 1 <= port <= 65535:
        raise ValueError("Invalid API port")
    return parsed.hostname, port


def node(value):
    if not isinstance(value, dict) or set(value) != {"host", "user", "api_url"}:
        raise ValueError("Every approved node requires exactly host, user and api_url")
    if not isinstance(value["host"], str) or not isinstance(value["user"], str):
        raise ValueError("Deployment host and SSH user must be strings")
    address = ipaddress.IPv4Address(value["host"])
    if address.is_loopback or address.is_multicast or address.is_unspecified or not re.fullmatch(r"[a-z_][a-z0-9_-]{0,31}", value["user"]):
        raise ValueError("Invalid deployment host or SSH user")
    health_address(value["api_url"])
    return value


def run(arguments, timeout=60):
    return subprocess.run(arguments, check=True, capture_output=True, text=True, timeout=timeout).stdout.strip()


def bundles():
    revision = required("GITHUB_SHA")
    if not re.fullmatch(r"[a-f0-9]{40}", revision):
        raise ValueError("Invalid build revision")
    metadata = json.loads(required("OMNI_ARTIFACTS"))
    if set(metadata) != set(ARCHES):
        raise ValueError("Invalid artifact identity")
    result = {}
    for arch in ARCHES:
        identity = metadata[arch]
        if type(identity.get("id")) is not int or identity["id"] <= 0 or not re.fullmatch(r"sha256:[a-f0-9]{64}", identity.get("digest", "")):
            raise ValueError("Invalid artifact identity")
        actual = github(f"actions/artifacts/{identity['id']}")
        if actual.get("digest") != identity["digest"] or actual.get("name") != f"omni_linux_{arch}" or actual.get("expired") is not False:
            raise ValueError("Artifact identity changed after CI")
        directory = Path("artifacts") / f"omni_linux_{arch}"
        if any(not (directory / name).is_file() or (directory / name).is_symlink() for name in ("omni", "SHA256SUMS", "REVISION")):
            raise ValueError("Incomplete artifact bundle")
        checksum = (directory / "SHA256SUMS").read_text().split()
        if len(checksum) != 2 or checksum[1] != "omni" or not re.fullmatch(r"[a-f0-9]{64}", checksum[0]) or digest(directory / "omni") != checksum[0] or (directory / "REVISION").read_text().strip() != revision:
            raise ValueError("Binary checksum or revision does not match CI")
        result[arch] = (directory / "omni", checksum[0])
    return result


def rollout(stage):
    check_environment("omni-canary" if stage == "canary" else "omni-production")
    artifacts = bundles()
    if stage == "canary":
        nodes = [node({"host": CANARY, "user": "root", "api_url": required("OMNI_CANARY_API_URL")})]
    else:
        values = json.loads(required("OMNI_DEPLOY_NODES"))
        if not isinstance(values, list) or not values:
            raise ValueError("An explicit approved fleet inventory is required")
        nodes = [node(value) for value in values]
        if len({value["host"] for value in nodes}) != len(nodes):
            raise ValueError("Duplicate fleet hosts")
        nodes = [value for value in nodes if value["host"] != CANARY]
        if not nodes:
            raise ValueError("Fleet contains no nodes beyond the canary")
    with tempfile.TemporaryDirectory(prefix="omni-ssh-") as workspace:
        directory = Path(workspace)
        key, known, config = (directory / name for name in ("key", "known_hosts", "config"))
        key.write_text(required("SSH_PRIVATE_KEY") + "\n")
        key.chmod(0o600)
        known.write_text(required("SSH_KNOWN_HOSTS") + "\n")
        lines = [f'Host *\n Port 2222\n BatchMode yes\n IdentitiesOnly yes\n StrictHostKeyChecking yes\n ConnectTimeout 15\n ServerAliveInterval 15\n ServerAliveCountMax 3\n IdentityFile "{key}"\n UserKnownHostsFile "{known}"\n', f"Host omni-jump\n HostName {CANARY}\n User root\n"]
        for index, value in enumerate(nodes):
            lines.append(f"Host omni-{index}\n HostName {value['host']}\n User {value['user']}\n")
            if value["host"] in {f"5.166.219.{last}" for last in range(51, 57)} | {"95.79.98.110"}:
                lines.append(" ProxyJump omni-jump\n")
        config.write_text("".join(lines))
        for index, value in enumerate(nodes):
            print(f"Updating approved node {value['host']}", flush=True)
            ssh = ["ssh", "-F", str(config), f"omni-{index}"]
            architecture = {"x86_64": "amd64", "aarch64": "arm64"}.get(run(ssh + ["uname -m"]))
            if architecture not in artifacts:
                raise ValueError("Unsupported node architecture")
            binary, checksum = artifacts[architecture]
            print(f"Revision {required('GITHUB_SHA')}; {architecture} binary SHA-256 {checksum}", flush=True)
            remote = run(ssh + ["mktemp -d /tmp/omni-ci.XXXXXXXX"])
            if not re.fullmatch(r"/tmp/omni-ci\.[A-Za-z0-9]{8}", remote):
                raise ValueError("Unexpected remote staging path")
            failed = False
            try:
                run(["scp", "-F", str(config), str(binary), str(Path(__file__).resolve()), f"omni-{index}:{remote}/"])
                command = ([] if value["user"] == "root" else ["sudo", "-n"]) + ["python3", f"{remote}/omni.py", "install", "--artifact", f"{remote}/omni", "--sha256", checksum, "--api-url", value["api_url"]]
                print(run(ssh + [shlex.join(command)], timeout=720), flush=True)
            except BaseException as error:
                failed = True
                if isinstance(error, subprocess.SubprocessError):
                    for captured in (getattr(error, "stdout", ""), getattr(error, "stderr", "")):
                        if isinstance(captured, bytes):
                            captured = captured.decode("utf-8", errors="replace")
                        for line in (captured or "").splitlines():
                            print("Remote diagnostic: " + re.sub(r"[\x00-\x1f\x7f]", " ", line), file=sys.stderr)
                raise
            finally:
                try:
                    run(ssh + [shlex.join(["rm", "-r", "--", remote])])
                except subprocess.SubprocessError:
                    if not failed:
                        raise
                    print("Staging cleanup failed; preserving the original deployment error", file=sys.stderr)


def game_state(ids=None):
    if ids is None:
        ids = run(["docker", "ps", "--filter", "label=Service=Pterodactyl", "--filter", "label=ContainerType=server_process", "--filter", "status=running", "--quiet", "--no-trunc"]).split()
    if not ids:
        return {}
    if any(not re.fullmatch(r"[a-f0-9]{64}", value) for value in ids):
        raise ValueError("Unexpected game container identity")
    rows = run(["docker", "inspect", "--format", "{{.Id}} {{.State.Running}} {{.State.StartedAt}} {{.RestartCount}}", *ids])
    state = {}
    for line in rows.splitlines():
        fields = line.split()
        if len(fields) != 4 or fields[0] not in ids or fields[1] != "true" or fields[0] in state:
            raise ValueError("Unexpected game container state")
        state[fields[0]] = fields[1:]
    if set(state) != set(ids):
        raise ValueError("A baseline game container is missing")
    return state


def probe(url):
    host, port = health_address(url)
    response = run(["curl", "--silent", "--show-error", "--max-time", "5", "--noproxy", "*", "--resolve", f"{host}:{port}:127.0.0.1", "--dump-header", "-", "--output", "/dev/null", "--write-out", "\n%{http_code}", url], timeout=10)
    return response.endswith("\n401") and re.search(r"(?im)^www-authenticate:\s*Bearer\s*$", response) and re.search(r"(?im)^x-request-id:\s*\S+", response)


def wait_ready(checksum, url):
    deadline, stable_pid, stable_count = time.monotonic() + 300, None, 0
    while time.monotonic() < deadline:
        try:
            run(["systemctl", "is-active", "--quiet", SERVICE])
            pid = run(["systemctl", "show", "--property=MainPID", "--value", SERVICE])
            if not pid.isdigit() or pid == "0" or digest(Path("/proc") / pid / "exe") != checksum or not probe(url):
                raise ValueError("Agent is not ready")
            stable_count = stable_count + 1 if pid == stable_pid else 1
            stable_pid = pid
            if stable_count >= 3:
                return
        except (OSError, ValueError, subprocess.SubprocessError):
            stable_count, stable_pid = 0, None
        time.sleep(5)
    raise RuntimeError("Omni did not become stably ready within 300 seconds")


@contextlib.contextmanager
def install_lock():
    import fcntl
    with open("/run/omni-deploy.lock", "w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield


@contextlib.contextmanager
def atomic_changes():
    # Deliver cancellation only after the on-disk replacement and recovery state agree.
    previous = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGTERM, signal.SIGHUP, signal.SIGINT})
    try:
        yield
    finally:
        signal.pthread_sigmask(signal.SIG_SETMASK, previous)


def install(artifact, checksum, url):
    if os.geteuid() != 0 or not re.fullmatch(r"[a-f0-9]{64}", checksum):
        raise ValueError("Installation requires root and a valid checksum")
    artifact = Path(artifact)
    health_address(url)
    if not artifact.is_file() or artifact.is_symlink() or digest(artifact) != checksum:
        raise ValueError("Staged binary checksum mismatch")
    with install_lock():
        if not BINARY.is_file() or BINARY.is_symlink():
            raise ValueError("Only an existing regular /usr/local/bin/omni installation can be updated")
        previous_stat = BINARY.stat()
        if previous_stat.st_uid != 0 or previous_stat.st_mode & 0o6022:
            raise ValueError("Existing binary must be root-owned without unsafe write/set-id permissions")
        run(["systemctl", "is-active", "--quiet", SERVICE])
        pid = run(["systemctl", "show", "--property=MainPID", "--value", SERVICE])
        old_checksum = digest(BINARY)
        if not pid.isdigit() or pid == "0" or digest(Path("/proc") / pid / "exe") != old_checksum or not probe(url):
            raise ValueError("Existing service/binary/API does not match the reviewed deployment layout")
        if old_checksum == checksum:
            print("Already running this verified binary; no restart")
            return
        games = game_state()
        candidate = backup = None
        applied = False
        def interrupted(signum, frame):
            raise InterruptedError("Deployment interrupted")
        handlers = {number: signal.signal(number, interrupted) for number in (signal.SIGTERM, signal.SIGHUP)}
        try:
            for kind in ("candidate", "backup"):
                descriptor, filename = tempfile.mkstemp(prefix=f".omni-{kind}-", dir=BINARY.parent)
                os.close(descriptor)
                path = Path(filename)
                if kind == "candidate":
                    candidate = path
                    shutil.copyfile(artifact, candidate)
                    os.chown(candidate, previous_stat.st_uid, previous_stat.st_gid)
                    candidate.chmod(stat.S_IMODE(previous_stat.st_mode))
                else:
                    backup = path
                    shutil.copy2(BINARY, backup)
                    os.chown(backup, previous_stat.st_uid, previous_stat.st_gid)
                with path.open("rb") as saved:
                    os.fsync(saved.fileno())
            with atomic_changes():
                os.replace(candidate, BINARY)
                candidate, applied = None, True
            run(["systemctl", "restart", SERVICE])
            wait_ready(checksum, url)
            if game_state(list(games)) != games:
                raise RuntimeError("A baseline game container changed; stopping rollout")
            with atomic_changes():
                os.replace(backup, BINARY.with_name("omni.previous"))
                backup, applied = None, False
            print(f"Verified binary, stable agent API and {len(games)} unchanged baseline game containers")
        except BaseException:
            if applied and backup is not None:
                try:
                    with atomic_changes():
                        os.replace(backup, BINARY)
                        backup, applied = None, False
                    run(["systemctl", "restart", SERVICE])
                    wait_ready(old_checksum, url)
                except BaseException as error:
                    raise RuntimeError(f"Rollback failed; previous binary retained at {backup or BINARY}") from error
            raise
        finally:
            for number, handler in handlers.items():
                signal.signal(number, handler)
            for path in (candidate, backup):
                if path is not None and not (path == backup and applied):
                    path.unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("stage", choices=("prepare", "canary", "fleet", "install"))
    parser.add_argument("--artifact")
    parser.add_argument("--sha256")
    parser.add_argument("--api-url")
    args = parser.parse_args()
    if args.stage == "prepare":
        prepare()
    elif args.stage == "install":
        if not all((args.artifact, args.sha256, args.api_url)):
            parser.error("install requires --artifact, --sha256 and --api-url")
        install(args.artifact, args.sha256, args.api_url)
    else:
        rollout(args.stage)


if __name__ == "__main__":
    main()
