#!/usr/bin/env python3
"""Preserve or rebuild the router around an authenticated Codex Sparkle update."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import plistlib
import shutil
import signal
import subprocess
import sys
import time
from pathlib import Path


SCRIPT_PATH = Path(__file__).resolve()
UPDATE_ROOT = SCRIPT_PATH.parent
CONFIG_PATH = UPDATE_ROOT / "config.json"
LOCK_PATH = UPDATE_ROOT / "active-update.json"
RUNS_ROOT = UPDATE_ROOT / "runs"
OPENAI_IDENTIFIER = "com.openai.codex"
OPENAI_TEAM = "2DC432GLL2"
HELPER_NAME = "Codex Subscription Router Computer Use.app"
POLL_SECONDS = 2
UPDATE_TIMEOUT_SECONDS = 20 * 60


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    prepare = subparsers.add_parser("prepare")
    prepare.add_argument("--app", type=Path, required=True)
    monitor = subparsers.add_parser("monitor")
    monitor.add_argument("--run", type=Path, required=True)
    return parser.parse_args()


def run(command: list[str], **kwargs: object) -> subprocess.CompletedProcess[str]:
    return subprocess.run(command, check=True, text=True, **kwargs)


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def read_json(path: Path) -> dict[str, object]:
    value = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(value, dict):
        raise RuntimeError(f"invalid object in {path}")
    return value


def write_json_atomic(path: Path, value: dict[str, object], mode: int = 0o600) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".new")
    temporary.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    temporary.chmod(mode)
    temporary.replace(path)


def read_plist(app: Path) -> dict[str, object]:
    with (app / "Contents" / "Info.plist").open("rb") as handle:
        value = plistlib.load(handle)
    if not isinstance(value, dict):
        raise RuntimeError(f"invalid Info.plist in {app}")
    return value


def signing_metadata(app: Path) -> tuple[str | None, str | None]:
    result = subprocess.run(
        ["codesign", "--display", "--verbose=4", str(app)],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    details = result.stdout + result.stderr
    identifier = None
    team = None
    for line in details.splitlines():
        if line.startswith("Identifier="):
            identifier = line.split("=", 1)[1].strip()
        elif line.startswith("TeamIdentifier="):
            team = line.split("=", 1)[1].strip()
    return identifier, None if team == "not set" else team


def verify_signed(app: Path, identifier: str, team: str | None) -> None:
    run(["codesign", "--verify", "--deep", "--strict", str(app)])
    actual_identifier, actual_team = signing_metadata(app)
    if actual_identifier != identifier or actual_team != team:
        raise RuntimeError(
            f"unexpected signature on {app}: identifier={actual_identifier!r}, "
            f"team={actual_team!r}"
        )


def configured_paths(config: dict[str, object]) -> tuple[Path, Path, Path]:
    try:
        project_root = Path(str(config["projectRoot"])).expanduser().resolve()
        destination = Path(str(config["destination"])).expanduser().resolve()
        helper = Path(str(config["helper"])).expanduser().resolve()
    except KeyError as error:
        raise RuntimeError(f"missing updater setting: {error.args[0]}") from error
    return project_root, destination, helper


def assert_tool_integrity(config: dict[str, object]) -> Path:
    project_root, _, _ = configured_paths(config)
    patcher = project_root / "scripts" / "patch_app.py"
    coordinator = project_root / "scripts" / "update_coordinator.py"
    if not patcher.is_file() or not coordinator.is_file():
        raise RuntimeError("the configured router source tree is unavailable")
    expected_patcher = str(config.get("patcherSha256", ""))
    expected_coordinator = str(config.get("coordinatorSha256", ""))
    if (
        sha256(patcher) != expected_patcher
        or sha256(coordinator) != expected_coordinator
        or sha256(SCRIPT_PATH) != expected_coordinator
    ):
        raise RuntimeError("router migration scripts failed their integrity check")
    expected_commit = str(config.get("projectCommit", ""))
    actual_commit = subprocess.check_output(
        ["git", "rev-parse", "HEAD"], cwd=project_root, text=True
    ).strip()
    if expected_commit == "" or actual_commit != expected_commit:
        raise RuntimeError("the configured router source commit has changed")
    tracked_changes = subprocess.check_output(
        ["git", "status", "--porcelain", "--untracked-files=no"],
        cwd=project_root,
        text=True,
    ).strip()
    if tracked_changes:
        raise RuntimeError("the configured router source has uncommitted tracked changes")
    return patcher


def process_ids_for(path: Path) -> list[int]:
    result = subprocess.run(
        ["pgrep", "-f", str(path)],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        text=True,
    )
    return [int(value) for value in result.stdout.split() if value.isdigit()]


def stop_processes(path: Path) -> None:
    pids = [pid for pid in process_ids_for(path) if pid != os.getpid()]
    for pid in pids:
        try:
            os.kill(pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline and process_ids_for(path):
        time.sleep(0.5)
    for pid in process_ids_for(path):
        if pid == os.getpid():
            continue
        try:
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass


def acquire_lock(run_root: Path) -> None:
    if LOCK_PATH.exists():
        try:
            previous = read_json(LOCK_PATH)
            pid = int(previous.get("pid", 0))
            if pid > 0:
                os.kill(pid, 0)
                raise RuntimeError("a Codex Router update migration is already active")
        except ProcessLookupError:
            pass
        except (OSError, ValueError, json.JSONDecodeError):
            pass
        LOCK_PATH.unlink(missing_ok=True)
    descriptor = os.open(LOCK_PATH, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
        json.dump({"pid": os.getpid(), "run": str(run_root)}, handle)


def release_lock() -> None:
    LOCK_PATH.unlink(missing_ok=True)


def copy_bundle(source: Path, destination: Path) -> None:
    if destination.exists():
        raise RuntimeError(f"backup destination already exists: {destination}")
    run(["ditto", str(source), str(destination)])


def prepare(app: Path) -> None:
    config = read_json(CONFIG_PATH)
    project_root, destination, helper = configured_paths(config)
    app = app.expanduser().resolve()
    if app != destination:
        raise RuntimeError(f"updater app path {app} does not match {destination}")
    info = read_plist(app)
    if not info.get("CodexMuxPatchedVersion"):
        raise RuntimeError("the running app is not a patched Codex Router build")
    assert_tool_integrity(config)

    run_id = time.strftime("%Y%m%d-%H%M%S") + f"-{os.getpid()}"
    run_root = RUNS_ROOT / run_id
    run_root.mkdir(mode=0o700, parents=True, exist_ok=False)
    acquire_lock(run_root)
    try:
        previous_app = run_root / "previous-router.app"
        copy_bundle(app, previous_app)
        identifier, team = signing_metadata(app)
        verify_signed(previous_app, identifier or OPENAI_IDENTIFIER, team)
        if helper.exists():
            copy_bundle(helper, run_root / "previous-helper.app")
        write_json_atomic(
            run_root / "state.json",
            {
                "destination": str(destination),
                "helper": str(helper),
                "oldBuild": str(info.get("CFBundleVersion", "")),
                "oldPatchedVersion": str(info.get("CodexMuxPatchedVersion", "")),
                "projectRoot": str(project_root),
            },
        )
        log_path = run_root / "migration.log"
        log = log_path.open("ab", buffering=0)
        process = subprocess.Popen(
            [sys.executable, str(SCRIPT_PATH), "monitor", "--run", str(run_root)],
            stdin=subprocess.DEVNULL,
            stdout=log,
            stderr=subprocess.STDOUT,
            start_new_session=True,
            close_fds=True,
        )
        log.close()
        write_json_atomic(
            LOCK_PATH,
            {"pid": process.pid, "run": str(run_root)},
        )
    except Exception:
        release_lock()
        raise


def updated_official_app_ready(app: Path, old_build: str) -> bool:
    try:
        info = read_plist(app)
        if info.get("CodexMuxPatchedVersion"):
            return False
        if str(info.get("CFBundleVersion", "")) == old_build:
            return False
        verify_signed(app, OPENAI_IDENTIFIER, OPENAI_TEAM)
        return True
    except (OSError, RuntimeError, subprocess.CalledProcessError, plistlib.InvalidFileException):
        return False


def wait_for_updated_app(app: Path, old_build: str) -> None:
    deadline = time.monotonic() + UPDATE_TIMEOUT_SECONDS
    stable_build = None
    stable_count = 0
    while time.monotonic() < deadline:
        if updated_official_app_ready(app, old_build):
            build = str(read_plist(app).get("CFBundleVersion", ""))
            if build == stable_build:
                stable_count += 1
            else:
                stable_build = build
                stable_count = 1
            if stable_count >= 3:
                return
        else:
            stable_build = None
            stable_count = 0
        time.sleep(POLL_SECONDS)
    raise TimeoutError("Sparkle did not produce a stable official Codex update")


def patch_command(
    patcher: Path,
    source: Path,
    destination: Path | None,
    config: dict[str, object],
) -> list[str]:
    command = [sys.executable, str(patcher), "--source", str(source)]
    if destination is None:
        command.append("--check-compatibility")
    else:
        command.extend(
            [
                "--destination",
                str(destination),
                "--skip-update-support",
            ]
        )
        if bool(config.get("allowAdhocSigning")):
            command.append("--allow-adhoc-signing")
    return command


def restore_previous(run_root: Path, destination: Path, helper: Path) -> None:
    stop_processes(destination)
    if destination.exists():
        destination.rename(run_root / "updated-incompatible.app")
    previous = run_root / "previous-router.app"
    if previous.exists():
        previous.rename(destination)
    previous_helper = run_root / "previous-helper.app"
    if not helper.exists() and previous_helper.exists():
        previous_helper.rename(helper)
    subprocess.run(["open", str(destination)], check=False)


def install_rebuilt(
    run_root: Path,
    destination: Path,
    helper: Path,
    staged_app: Path,
    staged_helper: Path,
) -> None:
    stop_processes(destination)
    destination.rename(run_root / "updated-official.app")
    if helper.exists():
        helper.rename(run_root / "replaced-helper.app")
    staged_app.rename(destination)
    staged_helper.rename(helper)
    subprocess.run(["open", str(destination)], check=False)


def monitor(run_root: Path) -> None:
    config = read_json(CONFIG_PATH)
    _, destination, helper = configured_paths(config)
    state = read_json(run_root / "state.json")
    old_build = str(state.get("oldBuild", ""))
    try:
        wait_for_updated_app(destination, old_build)
        patcher = assert_tool_integrity(config)
        environment = os.environ.copy()
        signing_identity = str(config.get("signingIdentity", "")).strip()
        if signing_identity:
            environment["CODEX_MUX_SIGNING_IDENTITY"] = signing_identity
        run(
            patch_command(patcher, destination, None, config),
            cwd=patcher.parent.parent,
            env=environment,
        )
        staging_root = run_root / "staging"
        staging_root.mkdir(mode=0o700)
        staged_app = staging_root / destination.name
        run(
            patch_command(patcher, destination, staged_app, config),
            cwd=patcher.parent.parent,
            env=environment,
        )
        staged_helper = staging_root / HELPER_NAME
        if not staged_app.is_dir() or not staged_helper.is_dir():
            raise RuntimeError("router rebuild did not produce both signed app bundles")
        install_rebuilt(
            run_root,
            destination,
            helper,
            staged_app,
            staged_helper,
        )
        print("Codex Router migration completed", flush=True)
    except TimeoutError as error:
        print(str(error), flush=True)
    except Exception as error:
        print(f"Codex Router migration skipped: {error}", flush=True)
        try:
            if destination.exists() and not read_plist(destination).get(
                "CodexMuxPatchedVersion"
            ):
                restore_previous(run_root, destination, helper)
        except Exception as restore_error:
            print(f"Codex Router rollback failed: {restore_error}", flush=True)
    finally:
        release_lock()


def main() -> int:
    args = parse_args()
    try:
        if args.command == "prepare":
            prepare(args.app)
        else:
            monitor(args.run.expanduser().resolve())
    except Exception as error:
        print(f"update coordinator failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
