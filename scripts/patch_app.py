#!/usr/bin/env python3
"""Create an independently signed ChatGPT.app copy with Codex multiplexing."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import plistlib
import re
import secrets
import shutil
import stat
import subprocess
import sys
import tempfile
import time
from pathlib import Path


PROJECT_ROOT = Path(__file__).resolve().parent.parent
PROJECT_VERSION = (PROJECT_ROOT / "VERSION").read_text(encoding="utf-8").strip()
DEFAULT_SOURCE = Path("/Applications/ChatGPT.app")
DEFAULT_DESTINATION = Path.home() / "Applications" / "Codex Subscription Router.app"
DEFAULT_STATE_ROOT = Path.home() / ".codex-mux"
DEFAULT_UPDATE_ROOT = DEFAULT_STATE_ROOT / "updater"
CONTROL_PORT = 48123
DESKTOP_PROFILE_NAME = "Codex Subscription Router"
# Sparkle only installs an app bundle whose identifier matches the running
# host.  The copy keeps the official identifier so an authenticated OpenAI
# update can land, while its launcher and userData path keep runtime state
# isolated from /Applications/ChatGPT.app.
DESKTOP_BUNDLE_IDENTIFIER = OPENAI_DESKTOP_CODE_IDENTIFIER = "com.openai.codex"
OPENAI_COMPUTER_USE_BUNDLE_IDENTIFIER = "com.openai.sky.CUAService"
COMPUTER_USE_BUNDLE_IDENTIFIER = "com.cdxmux.sky.CUAService"
COMPUTER_USE_DISPLAY_NAME = "Codex Subscription Router Computer Use"
COMPUTER_USE_APP_NAME = f"{COMPUTER_USE_DISPLAY_NAME}.app"
LAUNCH_SERVICES_REGISTER = Path(
    "/System/Library/Frameworks/CoreServices.framework/Frameworks/"
    "LaunchServices.framework/Support/lsregister"
)
ASAR_UNPACK_DIRECTORIES = (
    "node_modules/{@worklouder,better-sqlite3,node-mac-permissions,node-pty,objc-js}"
)
PREFERRED_SIGNING_IDENTITY_PREFIXES = (
    "Developer ID Application:",
    "Apple Development:",
)
OPENAI_INTERNAL_TEAM_IDENTIFIER = "HX7739G8FX"
OPENAI_DISTRIBUTION_TEAM_IDENTIFIER = "2DC432GLL2"
TESTED_SOURCE_BUILDS = {
    (
        "26.803.61601",
        "6396",
    ): "d5a44ed9e2f1db5f81dbbe85408aed256f3203c5b16f00817bb9d7cd941343cf",
    (
        "26.818.41509",
        "6962",
    ): "8eb91bd9efbf9a4dd04b9b0afdbfcb4e0bab5da18c1919ad74ca327c00c7e791",
}
LEGACY_RENDERER_PROFILE = "legacy-26.803"
CURRENT_RENDERER_PROFILE = "current-26.818"
PROFILE_LAYOUT_COUNTS = {
    LEGACY_RENDERER_PROFILE: {"asar_cua": 17, "package_cua": 49},
    CURRENT_RENDERER_PROFILE: {"asar_cua": 16, "package_cua": 99},
}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", action="version", version=f"%(prog)s {PROJECT_VERSION}")
    parser.add_argument("--source", type=Path, default=DEFAULT_SOURCE)
    parser.add_argument("--destination", type=Path, default=DEFAULT_DESTINATION)
    parser.add_argument(
        "--force",
        action="store_true",
        help="Replace an existing destination after moving it to a timestamped backup.",
    )
    parser.add_argument(
        "--allow-adhoc-signing",
        action="store_true",
        help="Allow an ad-hoc signature (Appshots and Computer Use may stop working).",
    )
    parser.add_argument(
        "--allow-untested-source",
        action="store_true",
        help="Deprecated compatibility alias; unknown builds are structurally analyzed.",
    )
    parser.add_argument(
        "--check-compatibility",
        action="store_true",
        help="Analyze the source without creating or replacing an application.",
    )
    parser.add_argument(
        "--allow-signing-team-change",
        action="store_true",
        help="Replace an existing build signed by a different Apple team.",
    )
    parser.add_argument(
        "--skip-update-support",
        action="store_true",
        help=argparse.SUPPRESS,
    )
    return parser.parse_args()


def run(command: list[str], *, cwd: Path | None = None) -> None:
    subprocess.run(command, cwd=cwd, check=True)


def output(command: list[str], *, cwd: Path | None = None) -> str:
    return subprocess.check_output(command, cwd=cwd, text=True).strip()


def require_tool(name: str) -> None:
    if shutil.which(name) is None:
        raise RuntimeError(f"required tool not found: {name}")


def resolve_signing_identity(allow_adhoc: bool) -> str:
    configured = os.environ.get("CODEX_MUX_SIGNING_IDENTITY", "").strip()
    if configured:
        if not signing_identity_usable(configured):
            raise RuntimeError(
                f"configured code-signing identity cannot sign code: {configured}"
            )
        return configured
    identities = output(["security", "find-identity", "-v", "-p", "codesigning"])
    available = re.findall(
        r'^\s*\d+\)\s+[0-9A-F]+\s+"([^"]+)"',
        identities,
        re.MULTILINE,
    )
    for prefix in PREFERRED_SIGNING_IDENTITY_PREFIXES:
        for identity in available:
            if identity.startswith(prefix) and signing_identity_usable(identity):
                return identity
    if allow_adhoc:
        print(
            "Warning: using an ad-hoc signature; Appshots and Computer Use may be unavailable.",
            file=sys.stderr,
        )
        return "-"
    raise RuntimeError(
        "no usable team-backed code-signing identity found; set CODEX_MUX_SIGNING_IDENTITY "
        "or explicitly pass --allow-adhoc-signing"
    )


def signing_identity_usable(identity: str) -> bool:
    return probe_signing_team(identity) is not None


def probe_signing_team(identity: str) -> str | None:
    with tempfile.TemporaryDirectory(prefix=".codex-mux-signing-probe-") as temporary:
        probe = Path(temporary) / "probe"
        shutil.copyfile("/usr/bin/true", probe)
        probe.chmod(0o755)
        result = subprocess.run(
            [
                "codesign",
                "--force",
                "--sign",
                identity,
                "--timestamp=none",
                str(probe),
            ],
            check=False,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        if result.returncode != 0:
            return None
        metadata = subprocess.run(
            ["codesign", "--display", "--verbose=4", str(probe)],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        match = re.search(
            r"^TeamIdentifier=(.+)$",
            metadata.stdout + metadata.stderr,
            re.MULTILINE,
        )
        if match is None:
            return None
        team = match.group(1).strip()
        return None if team == "not set" else team


def signing_team_identifier(identity: str) -> str | None:
    if identity == "-":
        return None
    team = probe_signing_team(identity)
    if team is None or re.fullmatch(r"[A-Z0-9]{10}", team) is None:
        raise RuntimeError(f"could not determine the signing team for {identity}")
    return team


def signed_code_metadata(path: Path) -> tuple[str | None, str | None]:
    result = subprocess.run(
        ["codesign", "--display", "--verbose=4", str(path)],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    details = result.stdout + result.stderr
    identifier_match = re.search(r"^Identifier=(.+)$", details, re.MULTILINE)
    team_match = re.search(r"^TeamIdentifier=(.+)$", details, re.MULTILINE)
    identifier = identifier_match.group(1).strip() if identifier_match else None
    team = team_match.group(1).strip() if team_match else None
    if team == "not set":
        team = None
    return identifier, team


def verify_signed_code(
    path: Path,
    expected_identifier: str,
    expected_team: str | None,
) -> None:
    run(["codesign", "--verify", "--deep", "--strict", str(path)])
    identifier, team = signed_code_metadata(path)
    if identifier != expected_identifier:
        raise RuntimeError(
            f"unexpected signing identifier on {path}: {identifier!r}"
        )
    if team != expected_team:
        raise RuntimeError(f"unexpected signing team on {path}: {team!r}")


def existing_signing_team(path: Path) -> str | None:
    if not path.exists():
        return None
    plist_path = path / "Contents" / "Info.plist"
    if plist_path.is_file():
        try:
            with plist_path.open("rb") as handle:
                recorded = plistlib.load(handle).get("CodexMuxSigningTeamIdentifier")
            if isinstance(recorded, str) and recorded != "":
                return None if recorded == "adhoc" else recorded
        except (OSError, plistlib.InvalidFileException):
            pass
    _, team = signed_code_metadata(path)
    return team


def ensure_components_are_stopped(paths: tuple[Path, ...]) -> None:
    for path in paths:
        if not path.exists():
            continue
        result = subprocess.run(
            ["pgrep", "-f", str(path)],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        if result.returncode == 0 and result.stdout.strip():
            raise RuntimeError(
                f"quit the running component before replacing it: {path}"
            )


MACH_O_MAGICS = {
    b"\xfe\xed\xfa\xce",  # 32-bit, big endian
    b"\xfe\xed\xfa\xcf",  # 64-bit, big endian
    b"\xce\xfa\xed\xfe",  # 32-bit, little endian
    b"\xcf\xfa\xed\xfe",  # 64-bit, little endian
    b"\xca\xfe\xba\xbe",  # universal binary
    b"\xbe\xba\xfe\xca",  # universal binary, little endian
}


def is_mach_o(path: Path) -> bool:
    if not path.is_file() or path.is_symlink():
        return False
    try:
        with path.open("rb") as handle:
            return handle.read(4) in MACH_O_MAGICS
    except OSError:
        return False


def arm64_swift_small_string(value: str) -> bytes:
    """Encode the instructions used to materialize a 10-byte Swift string."""
    encoded = value.encode("ascii")
    if len(encoded) != 10:
        raise ValueError("a signing team identifier must contain 10 ASCII bytes")

    def instruction(base: int, immediate: int, register: int, shift: int = 0) -> bytes:
        word = base | ((shift // 16) << 21) | (immediate << 5) | register
        return word.to_bytes(4, "little")

    chunks = [
        int.from_bytes(encoded[index : index + 2], "little")
        for index in range(0, len(encoded), 2)
    ]
    return b"".join(
        (
            instruction(0xD2800000, chunks[0], 0),
            instruction(0xF2800000, chunks[1], 0, 16),
            instruction(0xF2800000, chunks[2], 0, 32),
            instruction(0xF2800000, chunks[3], 0, 48),
            instruction(0xD2800000, chunks[4], 1),
            instruction(0xF2800000, 0xEA00, 1, 48),
        )
    )


def replace_same_length_identifier(
    path: Path, original: str, replacement: str
) -> int:
    """Replace an embedded identifier without changing binary or bundle offsets."""
    original_bytes = original.encode("ascii")
    replacement_bytes = replacement.encode("ascii")
    if len(original_bytes) != len(replacement_bytes):
        raise RuntimeError("replacement identifiers must have the same byte length")
    data = path.read_bytes()
    count = data.count(original_bytes)
    if count:
        path.write_bytes(data.replace(original_bytes, replacement_bytes))
    return count


def computer_use_package(app: Path) -> Path:
    return (
        app
        / "Contents"
        / "Resources"
        / "cua_node"
        / "lib"
        / "node_modules"
        / "@oai"
        / "sky"
    )


def retire_stale_cached_computer_use_app() -> None:
    """Move aside only a prior custom helper copied into the shared Codex home."""
    cached_app = (
        Path.home() / ".codex" / "computer-use" / "Codex Computer Use.app"
    )
    plist_path = cached_app / "Contents" / "Info.plist"
    if not plist_path.is_file():
        return
    try:
        with plist_path.open("rb") as handle:
            bundle_identifier = plistlib.load(handle).get("CFBundleIdentifier")
    except (OSError, plistlib.InvalidFileException):
        return
    if bundle_identifier != COMPUTER_USE_BUNDLE_IDENTIFIER:
        return
    if LAUNCH_SERVICES_REGISTER.is_file():
        run([str(LAUNCH_SERVICES_REGISTER), "-u", str(cached_app)])
    backup = cached_app.with_name(
        f"Codex Computer Use backup-{time.strftime('%Y%m%d-%H%M%S')}"
    )
    cached_app.rename(backup)
    print(f"Stale cached Computer Use helper moved to {backup}")


def patch_computer_use_identity(
    app: Path,
    team_identifier: str | None,
    renderer_profile: str,
) -> None:
    """Give the copied CUA service an independent identity and trusted callers."""
    package = computer_use_package(app)
    service = package / "Codex Computer Use.app"
    executable = service / "Contents" / "MacOS" / "SkyComputerUseService"
    if not executable.is_file():
        raise RuntimeError("bundled Codex Computer Use service was not found")

    for profile in package.rglob("embedded.provisionprofile"):
        profile.unlink()

    identifier_replacements = 0
    for candidate in package.rglob("*"):
        if candidate.is_file() and not candidate.is_symlink():
            identifier_replacements += replace_same_length_identifier(
                candidate,
                OPENAI_COMPUTER_USE_BUNDLE_IDENTIFIER,
                COMPUTER_USE_BUNDLE_IDENTIFIER,
            )
    expected_replacements = PROFILE_LAYOUT_COUNTS[renderer_profile]["package_cua"]
    if identifier_replacements != expected_replacements:
        raise RuntimeError(
            "expected "
            f"{expected_replacements} Computer Use identity "
            f"references, found {identifier_replacements}"
        )

    plist_path = service / "Contents" / "Info.plist"
    with plist_path.open("rb") as handle:
        info = plistlib.load(handle)
    info["CFBundleIdentifier"] = COMPUTER_USE_BUNDLE_IDENTIFIER
    info["CFBundleDisplayName"] = COMPUTER_USE_DISPLAY_NAME
    info["CFBundleName"] = COMPUTER_USE_DISPLAY_NAME
    for key in list(info):
        if key.startswith("SU"):
            del info[key]
    with plist_path.open("wb") as handle:
        plistlib.dump(info, handle, fmt=plistlib.FMT_BINARY, sort_keys=False)

    if team_identifier is None:
        return
    binary = executable.read_bytes()
    replacement = arm64_swift_small_string(team_identifier)
    for original_team, description in (
        (OPENAI_INTERNAL_TEAM_IDENTIFIER, "internal"),
        (OPENAI_DISTRIBUTION_TEAM_IDENTIFIER, "distribution"),
    ):
        original = arm64_swift_small_string(original_team)
        match_count = binary.count(original)
        if match_count != 2:
            raise RuntimeError(
                f"expected two Computer Use {description}-team checks, "
                f"found {match_count}; the official app layout may have changed"
            )
        binary = binary.replace(original, replacement)

        raw_original = original_team.encode("ascii")
        raw_replacement = team_identifier.encode("ascii")
        raw_match_count = binary.count(raw_original)
        expected_raw_matches = 1 if description == "internal" else 17
        if raw_match_count != expected_raw_matches:
            raise RuntimeError(
                f"expected {expected_raw_matches} Computer Use {description}-team "
                f"constants, found {raw_match_count}; the official app layout may have changed"
            )
        binary = binary.replace(raw_original, raw_replacement)

    original_bundle_id = b"com.openai.codex\0"
    replacement_bundle_id = DESKTOP_BUNDLE_IDENTIFIER.encode("ascii") + b"\0"
    if len(replacement_bundle_id) != len(original_bundle_id):
        raise RuntimeError(
            "the desktop bundle identifier must match the CUA identifier length"
        )
    if binary.count(original_bundle_id) != 1:
        raise RuntimeError("could not find the Computer Use production bundle ID")
    executable.write_bytes(binary.replace(original_bundle_id, replacement_bundle_id))


def patch_asar_computer_use_identity(
    extracted: Path,
    renderer_profile: str,
) -> None:
    """Keep desktop launch, temp-file, and service references on the new CUA ID."""
    replacements = 0
    for candidate in extracted.rglob("*"):
        if candidate.is_file() and not candidate.is_symlink():
            replacements += replace_same_length_identifier(
                candidate,
                OPENAI_COMPUTER_USE_BUNDLE_IDENTIFIER,
                COMPUTER_USE_BUNDLE_IDENTIFIER,
            )
    expected_replacements = PROFILE_LAYOUT_COUNTS[renderer_profile]["asar_cua"]
    if replacements != expected_replacements:
        raise RuntimeError(
            "expected "
            f"{expected_replacements} Computer Use references "
            f"in app.asar, found {replacements}"
        )


def sign_native_code_tree(root: Path, identity: str) -> None:
    """Sign native modules before ASAR records their final sizes."""
    if not root.is_dir():
        return
    for candidate in root.rglob("*"):
        if not is_mach_o(candidate):
            continue
        run(
            [
                "codesign",
                "--force",
                "--sign",
                identity,
                "--timestamp=none",
                "--options",
                "runtime",
                str(candidate),
            ]
        )


TEAM_SCOPED_ENTITLEMENTS = (
    "com.apple.application-identifier",
    "com.apple.developer.team-identifier",
    "com.apple.security.application-groups",
    "keychain-access-groups",
)


def sanitized_runtime_entitlements(executable: Path) -> dict[str, object] | None:
    """Keep runtime capabilities while removing the official app's team grants."""
    result = subprocess.run(
        ["codesign", "--display", "--entitlements", ":-", str(executable)],
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    if not result.stdout.strip():
        return None
    try:
        entitlements = plistlib.loads(result.stdout)
    except plistlib.InvalidFileException as error:
        raise RuntimeError(
            f"could not read signing entitlements from {executable}"
        ) from error
    if not isinstance(entitlements, dict):
        raise RuntimeError(f"invalid signing entitlements on {executable}")
    for key in TEAM_SCOPED_ENTITLEMENTS:
        entitlements.pop(key, None)
    return entitlements or None


AUTO_ENTITLEMENTS = object()


def sign_runtime_executable(
    executable: Path,
    identity: str,
    identifier: str | None = None,
    entitlements: dict[str, object] | None | object = AUTO_ENTITLEMENTS,
    runtime: bool = True,
) -> None:
    """Re-sign an embedded runtime without breaking JIT-backed processes."""
    if entitlements is AUTO_ENTITLEMENTS:
        entitlements = sanitized_runtime_entitlements(executable)
    command = [
        "codesign",
        "--force",
        "--sign",
        identity,
        "--timestamp=none",
    ]
    if runtime:
        command.extend(("--options", "runtime"))
    if identifier is None:
        command.append("--preserve-metadata=identifier")
    else:
        command.extend(("--identifier", identifier))
    if entitlements is None:
        run([*command, str(executable)])
        return
    with tempfile.TemporaryDirectory(prefix=".codesign-entitlements-") as temporary:
        entitlements_path = Path(temporary) / "entitlements.plist"
        with entitlements_path.open("wb") as handle:
            plistlib.dump(
                entitlements,
                handle,
                fmt=plistlib.FMT_XML,
                sort_keys=True,
            )
        run([*command, "--entitlements", str(entitlements_path), str(executable)])


def bundle_main_executable(bundle: Path) -> Path | None:
    plist_path = bundle / "Contents" / "Info.plist"
    executable_root = bundle / "Contents" / "MacOS"
    if bundle.suffix == ".framework":
        plist_path = bundle / "Versions" / "Current" / "Resources" / "Info.plist"
        executable_root = bundle / "Versions" / "Current"
    if not plist_path.is_file():
        return None
    with plist_path.open("rb") as handle:
        executable_name = plistlib.load(handle).get("CFBundleExecutable")
    if not isinstance(executable_name, str) or executable_name == "":
        return None
    executable = executable_root / executable_name
    return executable if executable.is_file() else None


def sign_runtime_bundle(
    bundle: Path,
    identity: str,
    identifier: str | None = None,
    entitlements: dict[str, object] | None | object = AUTO_ENTITLEMENTS,
    runtime: bool = True,
) -> None:
    if entitlements is AUTO_ENTITLEMENTS:
        executable = bundle_main_executable(bundle)
        entitlements = (
            sanitized_runtime_entitlements(executable)
            if executable is not None
            else None
        )
    command = [
        "codesign",
        "--force",
        "--sign",
        identity,
        "--timestamp=none",
    ]
    if runtime:
        command.extend(("--options", "runtime"))
    if identifier is not None:
        command.extend(("--identifier", identifier))
    if entitlements is None:
        run([*command, str(bundle)])
        return
    with tempfile.TemporaryDirectory(prefix=".codesign-entitlements-") as temporary:
        entitlements_path = Path(temporary) / "entitlements.plist"
        with entitlements_path.open("wb") as handle:
            plistlib.dump(
                entitlements,
                handle,
                fmt=plistlib.FMT_XML,
                sort_keys=True,
            )
        run([*command, "--entitlements", str(entitlements_path), str(bundle)])


def capture_computer_use_entitlements(
    app: Path,
) -> dict[Path, dict[str, object] | None]:
    service = computer_use_package(app) / "Codex Computer Use.app"
    if not service.is_dir():
        raise RuntimeError("bundled Codex Computer Use service was not found")
    return {
        executable.relative_to(service): sanitized_runtime_entitlements(executable)
        for executable in service.rglob("*")
        if is_mach_o(executable)
    }


def sign_computer_use_code(
    app: Path,
    identity: str,
    preserved_entitlements: dict[Path, dict[str, object] | None],
) -> None:
    """Keep the Computer Use service and its callers on one signing team."""
    resources = app / "Contents" / "Resources"
    service = computer_use_package(app) / "Codex Computer Use.app"
    if not service.is_dir():
        raise RuntimeError("bundled Codex Computer Use service was not found")

    for executable in sorted(
        (candidate for candidate in service.rglob("*") if is_mach_o(candidate)),
        key=lambda candidate: len(candidate.parts),
        reverse=True,
    ):
        relative = executable.relative_to(service)
        sign_runtime_executable(
            executable,
            identity,
            entitlements=preserved_entitlements.get(relative),
        )

    bundle_suffixes = {".app", ".appex", ".bundle", ".framework", ".xpc"}
    bundles = [
        candidate
        for candidate in service.rglob("*")
        if candidate.is_dir() and candidate.suffix in bundle_suffixes
    ]
    bundles.append(service)
    for bundle in sorted(
        set(bundles),
        key=lambda candidate: len(candidate.parts),
        reverse=True,
    ):
        identifier = (
            COMPUTER_USE_BUNDLE_IDENTIFIER if bundle == service else None
        )
        executable = bundle_main_executable(bundle)
        entitlements = (
            preserved_entitlements.get(executable.relative_to(service))
            if executable is not None
            else None
        )
        sign_runtime_bundle(bundle, identity, identifier, entitlements)
        run(["codesign", "--verify", "--deep", "--strict", str(bundle)])

    for executable_name in ("node", "node_repl"):
        executable = resources / "cua_node" / "bin" / executable_name
        sign_runtime_executable(executable, identity)
    sign_runtime_executable(
        app / "Contents" / "MacOS" / "ChatGPT",
        identity,
        OPENAI_DESKTOP_CODE_IDENTIFIER,
        runtime=False,
    )


def sign_sparkle_code(app: Path, identity: str) -> None:
    """Keep Sparkle's host, updater, and XPC helpers on the router's team."""
    framework_link = app / "Contents" / "Frameworks" / "Sparkle.framework"
    if not framework_link.is_dir():
        raise RuntimeError("the source app has no Sparkle framework")
    framework = framework_link.resolve()
    autoupdate = framework / "Autoupdate"
    updater = framework / "Updater.app"
    xpc_services = framework / "XPCServices"
    targets = (
        xpc_services / "Downloader.xpc",
        xpc_services / "Installer.xpc",
        updater,
    )
    if not autoupdate.is_file() or any(not target.is_dir() for target in targets):
        raise RuntimeError("the Sparkle updater layout is incomplete")
    sign_runtime_executable(autoupdate, identity)
    for target in targets:
        sign_runtime_bundle(target, identity)
        run(["codesign", "--verify", "--deep", "--strict", str(target)])
    sign_runtime_bundle(framework, identity)
    run(["codesign", "--verify", "--deep", "--strict", str(framework)])


def sign_independent_app(
    app: Path,
    identity: str,
    team_identifier: str | None,
    renderer_profile: str,
) -> None:
    """Apply one stable identity throughout the modified Electron bundle."""
    computer_use_entitlements = capture_computer_use_entitlements(app)
    patch_computer_use_identity(app, team_identifier, renderer_profile)
    sign_computer_use_code(app, identity, computer_use_entitlements)
    sign_sparkle_code(app, identity)
    run(
        [
            "codesign",
            "--force",
            "--sign",
            identity,
            "--timestamp=none",
            str(app / "Contents" / "Resources" / "codex"),
        ]
    )
    run(
        [
            "codesign",
            "--force",
            "--sign",
            identity,
            "--timestamp=none",
            str(app),
        ]
    )


def load_or_create_token() -> str:
    DEFAULT_STATE_ROOT.mkdir(mode=0o700, parents=True, exist_ok=True)
    DEFAULT_STATE_ROOT.chmod(0o700)
    token_path = DEFAULT_STATE_ROOT / "control-token"
    if token_path.exists():
        token = token_path.read_text(encoding="utf-8").strip()
        if re.fullmatch(r"[0-9a-f]{64}", token) is None:
            raise RuntimeError(f"invalid control token at {token_path}")
        token_path.chmod(0o600)
        return token
    token = secrets.token_hex(32)
    descriptor = os.open(token_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
        handle.write(token)
    return token


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def install_update_support(
    destination: Path,
    installed_computer_use_app: Path,
    signing_identity: str,
    allow_adhoc_signing: bool,
) -> None:
    tracked_changes = output(
        ["git", "status", "--porcelain", "--untracked-files=no"],
        cwd=PROJECT_ROOT,
    )
    if tracked_changes:
        raise RuntimeError(
            "commit tracked router changes before installing automatic update support"
        )
    project_commit = output(["git", "rev-parse", "HEAD"], cwd=PROJECT_ROOT)
    source_coordinator = PROJECT_ROOT / "scripts" / "update_coordinator.py"
    source_patcher = PROJECT_ROOT / "scripts" / "patch_app.py"
    DEFAULT_UPDATE_ROOT.mkdir(mode=0o700, parents=True, exist_ok=True)
    DEFAULT_UPDATE_ROOT.chmod(0o700)
    installed_coordinator = DEFAULT_UPDATE_ROOT / "update_coordinator.py"
    shutil.copy2(source_coordinator, installed_coordinator)
    installed_coordinator.chmod(0o700)
    config = {
        "allowAdhocSigning": allow_adhoc_signing,
        "destination": str(destination),
        "helper": str(installed_computer_use_app),
        "patcherSha256": sha256_file(source_patcher),
        "coordinatorSha256": sha256_file(source_coordinator),
        "projectCommit": project_commit,
        "projectRoot": str(PROJECT_ROOT),
        "projectVersion": PROJECT_VERSION,
        "signingIdentity": signing_identity,
    }
    config_path = DEFAULT_UPDATE_ROOT / "config.json"
    temporary = config_path.with_name(config_path.name + ".new")
    temporary.write_text(
        json.dumps(config, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    temporary.chmod(0o600)
    temporary.replace(config_path)


def build_proxy(destination: Path) -> None:
    destination.parent.mkdir(parents=True, exist_ok=True)
    run(
        [
            "go",
            "build",
            "-trimpath",
            "-ldflags=-s -w",
            "-o",
            str(destination),
            "./cmd/codex-mux",
        ],
        cwd=PROJECT_ROOT,
    )
    destination.chmod(destination.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)


def install_launcher(app: Path) -> None:
    """Pass Chromium its isolated profile before Electron's main process starts."""
    launcher = app / "Contents" / "MacOS" / "CodexSubscriptionRouterLauncher"
    run(
        [
            "xcrun",
            "clang",
            "-Os",
            "-Wall",
            "-Wextra",
            "-o",
            str(launcher),
            str(PROJECT_ROOT / "native" / "launcher.c"),
        ]
    )


def ensure_asar_tool() -> Path:
    asar = PROJECT_ROOT / "node_modules" / ".bin" / "asar"
    package_manifest = PROJECT_ROOT / "node_modules" / "@electron" / "asar" / "package.json"
    expected = json.loads(
        (PROJECT_ROOT / "package.json").read_text(encoding="utf-8")
    )["devDependencies"]["@electron/asar"]
    if not asar.exists() or not package_manifest.is_file():
        raise RuntimeError("run `npm ci --ignore-scripts` before patching")
    actual = json.loads(package_manifest.read_text(encoding="utf-8")).get("version")
    if actual != expected:
        raise RuntimeError(
            f"installed @electron/asar is {actual!r}, expected {expected!r}; "
            "run `npm ci --ignore-scripts`"
        )
    return asar


def replace_js_identifiers(source: str, replacements: dict[str, str]) -> str:
    pattern = re.compile(
        r"(?<![A-Za-z0-9_$])(" + "|".join(map(re.escape, replacements)) + r")(?![A-Za-z0-9_$])"
    )
    return pattern.sub(lambda match: replacements[match.group(1)], source)


def detect_renderer_profile(extracted: Path) -> str:
    bundles = list((extracted / "webview" / "assets").glob("app-initial-*.js"))
    if len(bundles) != 1:
        raise RuntimeError(
            f"expected one ChatGPT initial renderer bundle, found {len(bundles)}"
        )
    bundle = bundles[0].read_text(encoding="utf-8")
    profiles = []
    if "function wXc({sidebarFooter:e,triggerButton:t})" in bundle:
        profiles.append(LEGACY_RENDERER_PROFILE)
    if "function Oql(e){let t=(0,Nql.c)(253)" in bundle:
        profiles.append(CURRENT_RENDERER_PROFILE)
    if len(profiles) != 1:
        raise RuntimeError(
            "the ChatGPT renderer does not match exactly one supported structural profile"
        )
    return profiles[0]


def patch_current_renderer(extracted: Path, token: str) -> None:
    webview = extracted / "webview"
    index_path = webview / "index.html"
    index = index_path.read_text(encoding="utf-8")
    connect_anchor = "connect-src &#39;self&#39;"
    if index.count(connect_anchor) != 1:
        raise RuntimeError("could not find exactly one ChatGPT renderer CSP connect-src")
    index_path.write_text(
        index.replace(
            connect_anchor,
            f"{connect_anchor} http://127.0.0.1:{CONTROL_PORT}",
            1,
        ),
        encoding="utf-8",
    )

    bundles = list((webview / "assets").glob("app-initial-*.js"))
    if len(bundles) != 1:
        raise RuntimeError(
            f"expected one ChatGPT initial renderer bundle, found {len(bundles)}"
        )
    bundle_path = bundles[0]
    bundle = bundle_path.read_text(encoding="utf-8")
    if "function CodexMuxAccountMenu(" in bundle:
        raise RuntimeError("source app already contains the Codex multiplexer menu")

    component = (PROJECT_ROOT / "ui" / "account-menu.js").read_text(
        encoding="utf-8"
    )
    component = component.replace("__CODEX_MUX_CONTROL_PORT__", str(CONTROL_PORT))
    component = component.replace("__CODEX_MUX_CONTROL_TOKEN__", token)
    component = replace_js_identifiers(
        component,
        {
            "e7": "d7",
            "kXc": "Pql",
            "Lo": "ys",
            "_H": "mI",
            "S2": "CodexMuxPlusIcon",
            "BW": "VR",
            "CH": "bI",
            "QLs": "Bsc",
            "lt": "ct",
        },
    )
    component_anchor = "function Oql(e){let t=(0,Nql.c)(253)"
    if bundle.count(component_anchor) != 1:
        raise RuntimeError("could not find the native ChatGPT profile menu component")
    bundle = bundle.replace(component_anchor, component + "\n" + component_anchor, 1)

    protocol_anchors = (
        '"app/list":`apps`',
        '"app/installed":`apps`',
        '"app/read":`apps`',
        '"mcpServer/oauth/login":`mcp`',
        '"mcpServerStatus/list":`mcp`',
        "listMcpServers(e,t){let n=JSON.stringify({options:t,params:e})",
        "let i=this.sendRequest(`mcpServerStatus/list`,e,t);",
    )
    for anchor in protocol_anchors:
        if bundle.count(anchor) != 1:
            raise RuntimeError("could not verify the native Plugins RPC protocol")
    request_anchor = (
        "async sendRequest(e,t,n){if(this.dispatchMessage==null)throw Error("
        "`AppServerRequestClient is missing a message dispatcher`);"
    )
    if bundle.count(request_anchor) != 1:
        raise RuntimeError("could not find the native app-server request bridge")
    bundle = bundle.replace(
        request_anchor,
        "async sendRequest(e,t,n){t=codexMuxScopePluginRequest(e,t);"
        "if(this.dispatchMessage==null)throw Error("
        "`AppServerRequestClient is missing a message dispatcher`);",
        1,
    )

    profile_anchor = "function WGl(){let e=await B_.safeGet(`/wham/profiles/me`)"
    if bundle.count(profile_anchor) != 1:
        raise RuntimeError("could not find the native profile stats request")
    bundle = bundle.replace(
        profile_anchor,
        "function WGl(){let e=await codexMuxProfileData("
        "globalThis.__codexMuxSelectedProfileAccountId??null)",
        1,
    )

    usage_modal_anchor = "function Bsc(e){"
    if bundle.count(usage_modal_anchor) != 1:
        raise RuntimeError("could not find the native Usage modal component")
    bundle = bundle.replace(
        usage_modal_anchor,
        "function Bsc(e){CodexMuxUseResetAccountState();",
        1,
    )
    reset_query_anchor = (
        "function TCa(){let e=(0,MV.c)(1),t;return "
        "e[0]===Symbol.for(`react.memo_cache_sentinel`)?"
        "(t={queryKey:[`rate-limit-reset-credits`],queryFn:ECa,"
        "refetchInterval:jp.ONE_MINUTE,staleTime:jp.FIVE_SECONDS},e[0]=t):"
        "t=e[0],It(t)}"
    )
    if bundle.count(reset_query_anchor) != 1:
        raise RuntimeError("could not find the native reset-credit query")
    bundle = bundle.replace(
        reset_query_anchor,
        "function TCa(){let e=window.__codexMuxResetAccountId;return It({"
        "queryKey:[`rate-limit-reset-credits`,e??`primary`],"
        "queryFn:e?()=>codexMuxRateLimitResets(e):ECa,"
        "refetchInterval:jp.ONE_MINUTE,staleTime:jp.FIVE_SECONDS})}",
        1,
    )
    reset_mutation_anchor = (
        "function DCa(){let e=(0,MV.c)(3),t=ct(),n=vb(),r;return "
        "e[0]!==n||e[1]!==t?(r={mutationFn:OCa,onSuccess:(e,r)=>{"
        "let{creditId:i}=r,a=e.code;if(a===`reset`||a===`already_redeemed`){"
        "let n=e.code===`reset`?e.credit?.id??i:i;"
        "t.setQueryData([`rate-limit-reset-credits`],e=>ZSa(e,a,n))}"
        "Promise.all([n([`rate-limit-status`]),n([`rate-limit-reset-credits`])])}},"
        "e[0]=n,e[1]=t,e[2]=r):r=e[2],Qt(r)}"
    )
    if bundle.count(reset_mutation_anchor) != 1:
        raise RuntimeError("could not find the native reset-credit mutation")
    bundle = bundle.replace(
        reset_mutation_anchor,
        "function DCa(){let e=ct(),t=vb(),n=window.__codexMuxResetAccountId,"
        "r=[`rate-limit-reset-credits`,n??`primary`];return Qt({"
        "mutationFn:n?i=>codexMuxConsumeRateLimitReset(n,i):OCa,"
        "onSuccess:(n,i)=>{let{creditId:a}=i,o=n.code;"
        "if(o===`reset`||o===`already_redeemed`){let t=o===`reset`?"
        "n.credit?.id??a:a;e.setQueryData(r,e=>ZSa(e,o,t))}"
        "Promise.all([t([`rate-limit-status`]),t(r)])}})}",
        1,
    )

    replacements = (
        (
            "let y=v;if(g!=null){",
            "let y=window.__codexMuxSelectedUsageWindows??v;if(g!=null){",
            "native usage-window selection",
        ),
        (
            "_e=(0,u0.jsxs)(IR,{children:[he,ge]})",
            "_e=(0,u0.jsxs)(IR,{children:[he,ge,"
            "window.__codexMuxResetAccountSelector??null]})",
            "native Usage sheet header",
        ),
        (
            "usageItems:Ct",
            "usageItems:(0,d7.jsx)(CodexMuxAccountMenu,{})",
            "native ChatGPT usage menu slot",
        ),
    )
    for anchor, replacement, description in replacements:
        if bundle.count(anchor) != 1:
            raise RuntimeError(f"could not find the {description}")
        bundle = bundle.replace(anchor, replacement, 1)

    open_change_anchors = (
        "(0,d7.jsx)(hI,{align:`start`,contentStyle:y,open:s,side:`top`,"
        "sideOffset:6,triggerButton:Dt,onOpenChange:l,children:P})",
        "(0,d7.jsx)(hI,{open:s,onOpenChange:l,contentWidth:`panel`,"
        "triggerButton:Dt,children:Rt})",
    )
    for anchor in open_change_anchors:
        if bundle.count(anchor) != 1:
            raise RuntimeError("could not find a native profile menu open-state hook")
        bundle = bundle.replace(
            anchor,
            anchor.replace(
                "onOpenChange:l", "onOpenChange:CodexMuxProfileMenuOpenChange(l)"
            ),
            1,
        )

    for depleted_anchor in (
        "defaultMessage:`You’re out of Codex and Work usage`",
        "defaultMessage:`You’ve used all Codex and Work usage`",
        "defaultMessage:`You’ve reached your usage limit`",
    ):
        if bundle.count(depleted_anchor) != 1:
            raise RuntimeError("could not find a native subscription depletion alert")
        bundle = bundle.replace(
            depleted_anchor,
            "defaultMessage:`All connected subscriptions are depleted`",
            1,
        )
    bundle_path.write_text(bundle, encoding="utf-8")

    profile_bundles = list((webview / "assets").glob("profile-*.js"))
    if len(profile_bundles) != 1:
        raise RuntimeError(
            f"expected one native Profile settings bundle, found {len(profile_bundles)}"
        )
    profile_path = profile_bundles[0]
    profile = profile_path.read_text(encoding="utf-8")
    profile_anchor = (
        "Yt=(0,$.jsx)(`section`,{\"aria-busy\":Kt,"
        "className:`flex flex-col items-center`,children:Jt})"
    )
    if profile.count(profile_anchor) != 1:
        raise RuntimeError("could not find the native Profile identity section")
    profile = profile.replace(
        profile_anchor,
        "Yt=(0,$.jsx)(`section`,{\"aria-busy\":Kt,"
        "className:`flex flex-col items-center`,children:"
        "globalThis.CodexMuxProfileAvatarStack?.({onSelect:()=>M.refetch()})??Jt})",
        1,
    )
    profile_path.write_text(profile, encoding="utf-8")

    plugin_scope_anchor = "action:F,children:w})"
    plugin_candidates = [
        path
        for path in (webview / "assets").glob("plugins-settings-*.js")
        if plugin_scope_anchor in path.read_text(encoding="utf-8")
    ]
    if len(plugin_candidates) != 1:
        raise RuntimeError(
            f"expected one native Plugins settings bundle, found {len(plugin_candidates)}"
        )
    plugin_path = plugin_candidates[0]
    plugin = plugin_path.read_text(encoding="utf-8")
    plugin_path.write_text(
        plugin.replace(
            plugin_scope_anchor,
            "action:F,children:[globalThis.CodexMuxPluginScope?.()??null,w]})",
            1,
        ),
        encoding="utf-8",
    )

    thread_component_anchor = "function VT(e){let t=(0,WT.c)(33)"
    thread_children_anchor = "children:[l,u,d,f,p,m,h,g,_,v,y,b,x,S,C]"
    thread_candidates = []
    for path in (webview / "assets").glob("local-conversation-thread-*.js"):
        content = path.read_text(encoding="utf-8")
        if thread_component_anchor in content and thread_children_anchor in content:
            thread_candidates.append((path, content))
    if len(thread_candidates) != 1:
        raise RuntimeError(
            "could not identify exactly one native thread summary renderer bundle"
        )
    thread_path, thread = thread_candidates[0]
    component = (PROJECT_ROOT / "ui" / "thread-subscription.js").read_text(
        encoding="utf-8"
    )
    component = component.replace("__CODEX_MUX_CONTROL_PORT__", str(CONTROL_PORT))
    component = component.replace("__CODEX_MUX_CONTROL_TOKEN__", token)
    component = replace_js_identifiers(
        component,
        {"$n": "$t", "sr": "ge", "TE": "gw", "zE": "GT", "K": "Z"},
    )
    thread = thread.replace(
        thread_component_anchor,
        component + "\n" + thread_component_anchor,
        1,
    )
    thread = thread.replace(
        thread_children_anchor,
        "children:[l,(0,GT.jsx)(CodexMuxThreadSubscription,{}),"
        "u,d,f,p,m,h,g,_,v,y,b,x,S,C]",
        1,
    )
    thread_path.write_text(thread, encoding="utf-8")


def patch_renderer(
    extracted: Path,
    token: str,
    renderer_profile: str,
) -> None:
    if renderer_profile == CURRENT_RENDERER_PROFILE:
        patch_current_renderer(extracted, token)
        return
    if renderer_profile != LEGACY_RENDERER_PROFILE:
        raise RuntimeError(f"unsupported renderer profile: {renderer_profile}")
    webview = extracted / "webview"
    index_path = webview / "index.html"
    index = index_path.read_text(encoding="utf-8")

    connect_anchor = "connect-src &#39;self&#39;"
    if connect_anchor not in index:
        raise RuntimeError("could not find ChatGPT renderer CSP connect-src")
    index = index.replace(
        connect_anchor,
        f"{connect_anchor} http://127.0.0.1:{CONTROL_PORT}",
        1,
    )
    index_path.write_text(index, encoding="utf-8")

    initial_bundles = list((webview / "assets").glob("app-initial-*.js"))
    if len(initial_bundles) != 1:
        raise RuntimeError(
            f"expected one ChatGPT initial renderer bundle, found {len(initial_bundles)}"
        )
    bundle_path = initial_bundles[0]
    bundle = bundle_path.read_text(encoding="utf-8")
    if "function CodexMuxAccountMenu(" in bundle:
        raise RuntimeError("source app already contains the Codex multiplexer menu")

    component = (PROJECT_ROOT / "ui" / "account-menu.js").read_text(encoding="utf-8")
    component = component.replace("__CODEX_MUX_CONTROL_PORT__", str(CONTROL_PORT))
    component = component.replace("__CODEX_MUX_CONTROL_TOKEN__", token)
    component_anchor = "function wXc({sidebarFooter:e,triggerButton:t})"
    if bundle.count(component_anchor) != 1:
        raise RuntimeError("could not find the native ChatGPT profile menu component")
    bundle = bundle.replace(component_anchor, component + "\n" + component_anchor, 1)

    plugin_rpc_mapping_anchors = (
        '"list-apps":q9((e,{priority:t,source:n,timeoutMs:r,trace:i,...a})=>'
        "e.sendRequest(`app/list`,a,",
        '"list-installed-apps":q9((e,t)=>e.sendRequest(`app/installed`,t))',
        '"read-apps":q9((e,t)=>e.sendRequest(`app/read`,t))',
        '"login-mcp-server":q9((e,t)=>'
        "e.sendRequest(`mcpServer/oauth/login`,t))",
        '"list-mcp-server-status":K9((e,{priority:t,source:n,timeoutMs:r,'
        "trace:i,...a})=>e.listMcpServers(a,",
        "listMcpServers(e,t){let n=JSON.stringify({options:t,params:e})",
        "let i=this.sendRequest(`mcpServerStatus/list`,e,t);",
    )
    for mapping_anchor in plugin_rpc_mapping_anchors:
        if bundle.count(mapping_anchor) != 1:
            raise RuntimeError(
                "could not verify the native Plugins request-to-RPC mapping"
            )

    app_server_request_anchor = (
        "function gm(e,t,n){return n==null?h6e.sendRequest(e,t):"
        "h6e.sendRequest(e,t,n)}"
    )
    if bundle.count(app_server_request_anchor) != 1:
        raise RuntimeError("could not find the native app-server request bridge")
    bundle = bundle.replace(
        app_server_request_anchor,
        "function gm(e,t,n){let r=codexMuxScopePluginRequest(e,t);"
        "return n==null?h6e.sendRequest(e,r):h6e.sendRequest(e,r,n)}",
        1,
    )

    profile_query_anchor = "let e=await T_.safeGet(`/wham/profiles/me`)"
    if bundle.count(profile_query_anchor) != 1:
        raise RuntimeError("could not find the native profile stats request")
    bundle = bundle.replace(
        profile_query_anchor,
        "let e=await codexMuxProfileData("
        "globalThis.__codexMuxSelectedProfileAccountId??null)",
        1,
    )

    native_usage_modal_anchor = "function QLs(e){"
    if bundle.count(native_usage_modal_anchor) != 1:
        raise RuntimeError("could not find the native Usage modal component")
    bundle = bundle.replace(
        native_usage_modal_anchor,
        "function QLs(e){CodexMuxUseResetAccountState();",
        1,
    )

    reset_query_anchor = (
        "function l6r(){let e=(0,$F.c)(1),t;return "
        "e[0]===Symbol.for(`react.memo_cache_sentinel`)?"
        "(t={queryKey:[`rate-limit-reset-credits`],queryFn:u6r,"
        "refetchInterval:vm.ONE_MINUTE,staleTime:vm.FIVE_SECONDS},e[0]=t):"
        "t=e[0],Lt(t)}"
    )
    if bundle.count(reset_query_anchor) != 1:
        raise RuntimeError("could not find the native reset-credit query")
    bundle = bundle.replace(
        reset_query_anchor,
        "function l6r(){let e=window.__codexMuxResetAccountId;return Lt({"
        "queryKey:[`rate-limit-reset-credits`,e??`primary`],"
        "queryFn:e?()=>codexMuxRateLimitResets(e):u6r,"
        "refetchInterval:vm.ONE_MINUTE,staleTime:vm.FIVE_SECONDS})}",
        1,
    )

    reset_mutation_anchor = (
        "function d6r(){let e=(0,$F.c)(3),t=lt(),n=zO(),r;return "
        "e[0]!==n||e[1]!==t?(r={mutationFn:f6r,onSuccess:(e,r)=>{"
        "let{creditId:i}=r,a=e.code;if(a===`reset`||a===`already_redeemed`){"
        "let n=e.code===`reset`?e.credit?.id??i:i;"
        "t.setQueryData([`rate-limit-reset-credits`],e=>F3r(e,a,n))}"
        "Promise.all([n([`rate-limit-status`]),n([`rate-limit-reset-credits`])])}},"
        "e[0]=n,e[1]=t,e[2]=r):r=e[2],$t(r)}"
    )
    if bundle.count(reset_mutation_anchor) != 1:
        raise RuntimeError("could not find the native reset-credit mutation")
    bundle = bundle.replace(
        reset_mutation_anchor,
        "function d6r(){let e=lt(),t=zO(),n=window.__codexMuxResetAccountId,"
        "r=[`rate-limit-reset-credits`,n??`primary`];return $t({"
        "mutationFn:n?i=>codexMuxConsumeRateLimitReset(n,i):f6r,"
        "onSuccess:(n,i)=>{let{creditId:a}=i,o=n.code;"
        "if(o===`reset`||o===`already_redeemed`){let t=o===`reset`?"
        "n.credit?.id??a:a;e.setQueryData(r,e=>F3r(e,o,t))}"
        "Promise.all([t([`rate-limit-status`]),t(r)])}})}",
        1,
    )

    selected_usage_anchor = "let y=v;if(g!=null){"
    if bundle.count(selected_usage_anchor) != 1:
        raise RuntimeError("could not find the native usage-window selection")
    bundle = bundle.replace(
        selected_usage_anchor,
        "let y=window.__codexMuxSelectedUsageWindows??v;if(g!=null){",
        1,
    )

    usage_header_anchor = (
        "let ve;t[46]===ge?ve=t[47]:"
        "(ve=(0,k2.jsxs)(LL,{children:[ge,_e]}),t[46]=ge,t[47]=ve);"
    )
    if bundle.count(usage_header_anchor) != 1:
        raise RuntimeError("could not find the native Usage sheet header")
    bundle = bundle.replace(
        usage_header_anchor,
        "let ve=(0,k2.jsxs)(LL,{children:[ge,_e,"
        "window.__codexMuxResetAccountSelector??null]});",
        1,
    )

    usage_anchor = "usageItems:Ge"
    if bundle.count(usage_anchor) != 1:
        raise RuntimeError("could not find the native ChatGPT usage menu slot")
    bundle = bundle.replace(
        usage_anchor,
        "usageItems:(0,e7.jsx)(CodexMuxAccountMenu,{})",
        1,
    )

    open_change_anchors = (
        "triggerButton:Ke,onOpenChange:o,children:(0,e7.jsx)(bXc",
        "return(0,e7.jsx)(vH,{open:a,onOpenChange:o,contentWidth:`panel`",
    )
    for anchor in open_change_anchors:
        if bundle.count(anchor) != 1:
            raise RuntimeError("could not find a native profile menu open-state hook")
        bundle = bundle.replace(
            anchor,
            anchor.replace(
                "onOpenChange:o",
                "onOpenChange:CodexMuxProfileMenuOpenChange(o)",
            ),
            1,
        )

    depleted_alert_anchors = (
        "defaultMessage:`You’re out of Codex and Work usage`",
        "defaultMessage:`You’ve used all Codex and Work usage`",
        "defaultMessage:`You’ve reached your usage limit`",
    )
    for depleted_anchor in depleted_alert_anchors:
        if bundle.count(depleted_anchor) != 1:
            raise RuntimeError("could not find a native subscription depletion alert")
        bundle = bundle.replace(
            depleted_anchor,
            "defaultMessage:`All connected subscriptions are depleted`",
            1,
        )
    bundle_path.write_text(bundle, encoding="utf-8")

    profile_bundles = list((webview / "assets").glob("profile-*.js"))
    if len(profile_bundles) != 1:
        raise RuntimeError(
            f"expected one native Profile settings bundle, found {len(profile_bundles)}"
        )
    profile_bundle_path = profile_bundles[0]
    profile_bundle = profile_bundle_path.read_text(encoding="utf-8")
    profile_avatar_anchor = (
        "children:[(0,$.jsxs)(`div`,{className:`relative mb-4 size-20`,children:["
    )
    if profile_bundle.count(profile_avatar_anchor) != 1:
        raise RuntimeError("could not find the native Profile avatar")
    profile_bundle = profile_bundle.replace(
        profile_avatar_anchor,
        "children:[globalThis.CodexMuxProfileAvatarStack?.("
        "{onSelect:()=>A.refetch()})??null,"
        "(0,$.jsxs)(`div`,{className:"
        "globalThis.CodexMuxProfileAvatarStack?"
        "`hidden`:`relative mb-4 size-20`,children:[",
        1,
    )

    profile_name_anchor = "className:`flex w-full justify-center`"
    if profile_bundle.count(profile_name_anchor) != 1:
        raise RuntimeError("could not find the native Profile display name")
    profile_bundle = profile_bundle.replace(
        profile_name_anchor,
        "className:globalThis.__codexMuxSelectedProfileAccountId&&!A.isFetching?"
        "`flex w-full justify-center`:`hidden`",
        1,
    )
    profile_identity_anchor = (
        "className:`mt-1 flex min-h-7 items-center gap-1.5 text-base leading-5 "
        "font-normal text-token-text-tertiary`"
    )
    if profile_bundle.count(profile_identity_anchor) != 1:
        raise RuntimeError("could not find the native Profile username and plan badge")
    profile_bundle = profile_bundle.replace(
        profile_identity_anchor,
        "className:globalThis.__codexMuxSelectedProfileAccountId&&!A.isFetching?"
        "`mt-1 flex min-h-7 items-center gap-1.5 text-base leading-5 font-normal "
        "text-token-text-tertiary`:`hidden`",
        1,
    )
    profile_bundle_path.write_text(profile_bundle, encoding="utf-8")

    plugin_scope_anchor = "action:F,children:w})"
    plugin_bundles = [
        path
        for path in (webview / "assets").glob("plugins-settings-*.js")
        if plugin_scope_anchor in path.read_text(encoding="utf-8")
    ]
    if len(plugin_bundles) != 1:
        raise RuntimeError(
            f"expected one native Plugins settings bundle, found {len(plugin_bundles)}"
        )
    plugin_bundle_path = plugin_bundles[0]
    plugin_bundle = plugin_bundle_path.read_text(encoding="utf-8")
    if plugin_bundle.count(plugin_scope_anchor) != 1:
        raise RuntimeError("could not find the native Plugins settings content")
    plugin_bundle = plugin_bundle.replace(
        plugin_scope_anchor,
        "action:F,children:[globalThis.CodexMuxPluginScope?.()??null,w]})",
        1,
    )
    plugin_bundle_path.write_text(plugin_bundle, encoding="utf-8")

    thread_bundles = list((webview / "assets").glob("local-conversation-thread-*.js"))
    if len(thread_bundles) != 1:
        raise RuntimeError(
            f"expected one local conversation renderer bundle, found {len(thread_bundles)}"
        )
    thread_bundle_path = thread_bundles[0]
    thread_bundle = thread_bundle_path.read_text(encoding="utf-8")
    thread_component = (PROJECT_ROOT / "ui" / "thread-subscription.js").read_text(
        encoding="utf-8"
    )
    thread_component = thread_component.replace(
        "__CODEX_MUX_CONTROL_PORT__", str(CONTROL_PORT)
    )
    thread_component = thread_component.replace("__CODEX_MUX_CONTROL_TOKEN__", token)
    thread_component_anchor = "function bE(){let e=(0,wE.c)(57)"
    if thread_bundle.count(thread_component_anchor) != 1:
        raise RuntimeError("could not find the native thread summary sources component")
    thread_bundle = thread_bundle.replace(
        thread_component_anchor,
        thread_component + "\n" + thread_component_anchor,
        1,
    )
    summary_children_anchor = "children:[c,l,u,d,f,p,m,h,g,_,v,y,b,x]"
    if thread_bundle.count(summary_children_anchor) != 1:
        raise RuntimeError("could not find the native thread summary section list")
    thread_bundle = thread_bundle.replace(
        summary_children_anchor,
        "children:[c,l,u,d,f,(0,zE.jsx)(CodexMuxThreadSubscription,{}),p,m,h,g,_,v,y,b,x]",
        1,
    )
    thread_bundle_path.write_text(thread_bundle, encoding="utf-8")


def patch_update_migration(extracted: Path) -> None:
    """Prepare before Sparkle quits, then let the official updater proceed."""
    build_root = extracted / ".vite" / "build"
    sink_anchor = (
        "c.setInstallUpdatesRequestedSink(()=>{"
        "this.options.onInstallUpdatesRequested?.({quitImmediately:!1})})"
    )
    candidates = []
    for candidate in build_root.glob("*.js"):
        content = candidate.read_text(encoding="utf-8")
        if sink_anchor in content:
            candidates.append((candidate, content))
    if len(candidates) != 1:
        raise RuntimeError(
            "could not find exactly one supported Sparkle install-request hook"
        )

    bundle_path, bundle = candidates[0]
    coordinator = json.dumps(str(DEFAULT_UPDATE_ROOT / "update_coordinator.py"))
    prepare_function = (
        "function codexMuxPrepareUpdate(e){try{let t=XH(`node:child_process`)."
        "spawnSync(`/usr/bin/python3`,["
        + coordinator
        + ",`prepare`,`--app`,e],{encoding:`utf8`,timeout:12e4});"
        "if(t.status===0)return!0;console.error(`Codex Router update preparation "
        "failed`,t.stderr||t.stdout||t.error);return!1}catch(e){console.error("
        "`Codex Router update preparation failed`,e);return!1}}\n"
    )
    class_anchor = "var aU=class{"
    if bundle.count(class_anchor) != 1:
        raise RuntimeError("could not find the supported Codex update manager")
    bundle = bundle.replace(class_anchor, prepare_function + class_anchor, 1)
    replacement = (
        "c.setInstallUpdatesRequestedSink(()=>{"
        "codexMuxPrepareUpdate((0,l.resolve)(process.resourcesPath,`..`,`..`))&&"
        "this.options.onInstallUpdatesRequested?.({quitImmediately:!1})})"
    )
    bundle = bundle.replace(sink_anchor, replacement, 1)
    bundle_path.write_text(bundle, encoding="utf-8")


def patch_desktop_profile(
    extracted: Path, installed_computer_use_app: Path
) -> None:
    """Give the copied Electron app its own user-data and single-instance scope."""
    bootstrap_files = list((extracted / ".vite" / "build").glob("bootstrap-*.js"))
    if len(bootstrap_files) != 1:
        raise RuntimeError(
            f"expected one ChatGPT bootstrap bundle, found {len(bootstrap_files)}"
        )

    bootstrap_path = bootstrap_files[0]
    bootstrap = bootstrap_path.read_text(encoding="utf-8")
    profile_pattern = re.compile(
        r"(?P<electron>[A-Za-z_$][\w$]*)\.app\.setPath\("
        r"`userData`,[A-Za-z_$][\w$]*\(\{"
        r"appDataPath:(?P=electron)\.app\.getPath\(`appData`\),"
        r"buildFlavor:[^,}]+,env:process\.env\}\)\)"
    )

    def replacement(match: re.Match[str]) -> str:
        electron = match.group("electron")
        computer_use_pipe = json.dumps(str(DEFAULT_STATE_ROOT / "computer-use.sock"))
        computer_use_app = json.dumps(str(installed_computer_use_app))
        return (
            f"process.env.SKY_CUA_SERVICE_NATIVE_PIPE_PATH={computer_use_pipe};"
            f"process.env.SKY_CUA_SERVICE_PATH={computer_use_app};"
            f"process.env.CODEX_ELECTRON_COMPUTER_USE_APP_PATH={computer_use_app};"
            "process.env.CODEX_ELECTRON_SKIP_COMPUTER_USE_CANONICAL_REFRESH=`1`;"
            f"{electron}.app.setPath(`userData`,"
            f"{electron}.app.getPath(`appData`)+`/{DESKTOP_PROFILE_NAME}`)"
        )

    bootstrap, replacements = profile_pattern.subn(replacement, bootstrap, count=1)
    if replacements != 1:
        raise RuntimeError("could not isolate the copied ChatGPT desktop profile")

    bootstrap_path.write_text(bootstrap, encoding="utf-8")

    patch_update_migration(extracted)

    main_files = list((extracted / ".vite" / "build").glob("main-*.js"))
    if len(main_files) != 1:
        raise RuntimeError(
            f"expected one ChatGPT desktop main bundle, found {len(main_files)}"
        )
    main_path = main_files[0]
    main = main_path.read_text(encoding="utf-8")
    managed_service_pattern = re.compile(
        r"(?P<prefix>[A-Za-z_$][\w$]*=new [A-Za-z_$][\w$]*\()"
        r"[A-Za-z_$][\w$]*\([A-Za-z_$][\w$]*\.codexHome\)"
        r"(?P<suffix>,\{onServiceAvailable:)"
    )
    main, managed_service_replacements = managed_service_pattern.subn(
        lambda match: (
            match.group("prefix")
            + json.dumps(str(installed_computer_use_app))
            + match.group("suffix")
        ),
        main,
        count=1,
    )
    if managed_service_replacements != 1:
        raise RuntimeError(
            "could not pin the managed Computer Use service to its installed app"
        )

    computer_use_instruction = (
        "Control desktop apps on macOS through Computer Use."
    )
    strict_computer_use_instruction = (
        "Control desktop apps on macOS through Computer Use via node_repl and "
        "@oai/sky only. Never use shell commands, open, AppleScript, osascript, "
        "JXA, System Events, or CGEvent synthesis for computer interactions or "
        "as a fallback. If Computer Use is unavailable, report the failure "
        "instead of using another automation method."
    )
    if main.count(computer_use_instruction) != 1:
        raise RuntimeError("could not find the Computer Use tool instruction")
    main = main.replace(
        computer_use_instruction,
        strict_computer_use_instruction,
        1,
    )
    ui_test_bridge = extracted / ".vite" / "build" / "ui-test-bridge.cjs"
    shutil.copy2(PROJECT_ROOT / "ui" / "ui-test-bridge.cjs", ui_test_bridge)
    main += (
        "\n;if(process.env.CODEX_MUX_UI_TESTS===`1`)"
        "require(require(`node:path`).join(__dirname,`ui-test-bridge.cjs`)).start();"
    )
    main_path.write_text(main, encoding="utf-8")


def patch_info_plist(
    app: Path,
    asar_path: Path,
    team_identifier: str | None,
    source_report: dict[str, str],
) -> None:
    plist_path = app / "Contents" / "Info.plist"
    with plist_path.open("rb") as handle:
        info = plistlib.load(handle)
    info["CFBundleDisplayName"] = "Codex Subscription Router"
    info["CFBundleName"] = "Codex Subscription Router"
    # Sparkle requires the update payload and running host identifiers to match.
    # The separate URL scheme, display name, launcher, and profile still prevent
    # normal runtime state from colliding with /Applications/ChatGPT.app.
    info["CFBundleIdentifier"] = DESKTOP_BUNDLE_IDENTIFIER
    info["CFBundleExecutable"] = "CodexSubscriptionRouterLauncher"
    info["BundleSigningBaseName"] = "CodexSubscriptionRouter"
    info["CodexMuxSigningTeamIdentifier"] = team_identifier or "adhoc"
    info["CodexMuxPatchedVersion"] = PROJECT_VERSION
    info["CodexMuxRendererProfile"] = source_report["renderer_profile"]
    info["CodexMuxSourceVersion"] = source_report["version"]
    info["CodexMuxSourceBuild"] = source_report["build"]
    info["CodexMuxSourceAsarSHA256"] = source_report["asar_sha256"]
    info["CrProductDirName"] = DESKTOP_PROFILE_NAME
    # Preserve the official Sparkle feed key and update preferences. The
    # install-request hook snapshots this build before allowing replacement.
    for url_type in info.get("CFBundleURLTypes", []):
        schemes = url_type.get("CFBundleURLSchemes", [])
        url_type["CFBundleURLSchemes"] = [
            "codex-subscription-router" if value == "codex" else value for value in schemes
        ]
    digest = hashlib.sha256(asar_path.read_bytes()).hexdigest()
    info["ElectronAsarIntegrity"] = {
        "Resources/app.asar": {"algorithm": "SHA256", "hash": digest}
    }
    with plist_path.open("wb") as handle:
        plistlib.dump(info, handle, fmt=plistlib.FMT_BINARY, sort_keys=False)


def check_computer_use_layout(source: Path, renderer_profile: str) -> None:
    package = computer_use_package(source)
    service = package / "Codex Computer Use.app"
    executable = service / "Contents" / "MacOS" / "SkyComputerUseService"
    if not executable.is_file():
        raise RuntimeError("bundled Codex Computer Use service was not found")

    needle = OPENAI_COMPUTER_USE_BUNDLE_IDENTIFIER.encode("ascii")
    identifier_references = 0
    for candidate in package.rglob("*"):
        if (
            candidate.is_file()
            and not candidate.is_symlink()
            and candidate.name != "embedded.provisionprofile"
        ):
            identifier_references += candidate.read_bytes().count(needle)
    expected_references = PROFILE_LAYOUT_COUNTS[renderer_profile]["package_cua"]
    if identifier_references != expected_references:
        raise RuntimeError(
            f"expected {expected_references} Computer Use identity references, "
            f"found {identifier_references}"
        )

    binary = executable.read_bytes()
    for original_team, description, raw_expected in (
        (OPENAI_INTERNAL_TEAM_IDENTIFIER, "internal", 1),
        (OPENAI_DISTRIBUTION_TEAM_IDENTIFIER, "distribution", 17),
    ):
        small_count = binary.count(arm64_swift_small_string(original_team))
        raw_count = binary.count(original_team.encode("ascii"))
        if small_count != 2 or raw_count != raw_expected:
            raise RuntimeError(
                f"unsupported Computer Use {description}-team layout "
                f"(small={small_count}, raw={raw_count})"
            )
    if binary.count(b"com.openai.codex\0") != 1:
        raise RuntimeError("could not verify the Computer Use desktop bundle ID check")


def analyze_source_compatibility(source: Path) -> dict[str, str]:
    source = source.expanduser().resolve()
    plist_path = source / "Contents" / "Info.plist"
    asar_path = source / "Contents" / "Resources" / "app.asar"
    if not source.is_dir() or not plist_path.is_file() or not asar_path.is_file():
        raise RuntimeError(f"not a ChatGPT app bundle: {source}")

    for tool in ("codesign", "lipo", "node"):
        require_tool(tool)
    verify_signed_code(
        source,
        OPENAI_DESKTOP_CODE_IDENTIFIER,
        OPENAI_DISTRIBUTION_TEAM_IDENTIFIER,
    )
    with plist_path.open("rb") as handle:
        source_info = plistlib.load(handle)
    executable_name = source_info.get("CFBundleExecutable")
    if not isinstance(executable_name, str) or executable_name == "":
        raise RuntimeError("the source app has no main executable")
    architectures = output(
        ["lipo", "-archs", str(source / "Contents" / "MacOS" / executable_name)]
    ).split()
    if "arm64" not in architectures:
        raise RuntimeError("the source ChatGPT app has no arm64 executable")

    version = str(source_info.get("CFBundleShortVersionString", "unknown"))
    build = str(source_info.get("CFBundleVersion", "unknown"))
    asar_sha256 = hashlib.sha256(asar_path.read_bytes()).hexdigest()
    known_hash = TESTED_SOURCE_BUILDS.get((version, build))
    if known_hash is not None and known_hash != asar_sha256:
        print(
            "Warning: the source build number is known but its signed ASAR hash "
            "has changed; requiring a full structural compatibility pass.",
            file=sys.stderr,
        )

    asar = ensure_asar_tool()
    with tempfile.TemporaryDirectory(prefix=".codex-mux-compatibility-") as temporary:
        extracted = Path(temporary) / "asar"
        run([str(asar), "extract", str(asar_path), str(extracted)])
        renderer_profile = detect_renderer_profile(extracted)
        patch_asar_computer_use_identity(extracted, renderer_profile)
        patch_desktop_profile(
            extracted,
            Path(temporary) / COMPUTER_USE_APP_NAME,
        )
        patch_renderer(extracted, "0" * 64, renderer_profile)
    check_computer_use_layout(source, renderer_profile)

    report = {
        "version": version,
        "build": build,
        "asar_sha256": asar_sha256,
        "renderer_profile": renderer_profile,
        "architectures": ",".join(architectures),
        "known_exact_build": str(known_hash == asar_sha256).lower(),
    }
    print(
        "Compatibility passed: "
        f"ChatGPT {version} ({build}), profile {renderer_profile}, "
        f"arm64, official team {OPENAI_DISTRIBUTION_TEAM_IDENTIFIER}"
    )
    return report


def patch_app(
    source: Path,
    destination: Path,
    force: bool,
    allow_adhoc_signing: bool,
    allow_untested_source: bool,
    allow_signing_team_change: bool,
    skip_update_support: bool,
) -> None:
    source = source.expanduser().resolve()
    destination = destination.expanduser().resolve()
    if not source.is_dir() or not (source / "Contents" / "Resources" / "app.asar").is_file():
        raise RuntimeError(f"not a ChatGPT app bundle: {source}")
    if source == destination:
        raise RuntimeError(
            "source and destination must be different; "
            "the original app is never patched in place"
        )
    if destination.exists() and not force:
        raise RuntimeError(
            f"destination exists: {destination} "
            "(pass --force to create a recoverable backup)"
        )

    if allow_untested_source:
        print(
            "Warning: --allow-untested-source is no longer needed; every source "
            "build is structurally analyzed.",
            file=sys.stderr,
        )
    source_report = analyze_source_compatibility(source)
    renderer_profile = source_report["renderer_profile"]

    for tool in ("ditto", "go", "npm", "security", "xcrun"):
        require_tool(tool)
    asar = ensure_asar_tool()
    token = load_or_create_token()
    signing_identity = resolve_signing_identity(allow_adhoc_signing)
    team_identifier = signing_team_identifier(signing_identity)
    if destination.exists():
        installed_team = existing_signing_team(destination)
        if installed_team != team_identifier and not allow_signing_team_change:
            raise RuntimeError(
                "the selected signing team differs from the installed build; "
                "reuse the prior identity or pass --allow-signing-team-change"
            )
    destination.parent.mkdir(parents=True, exist_ok=True)
    installed_computer_use_app = destination.parent / COMPUTER_USE_APP_NAME
    if force:
        ensure_components_are_stopped((destination, installed_computer_use_app))

    with tempfile.TemporaryDirectory(prefix=".codex-subscription-router-", dir=destination.parent) as temporary:
        temporary_path = Path(temporary)
        staged_app = temporary_path / destination.name
        staged_computer_use_app = temporary_path / COMPUTER_USE_APP_NAME
        extracted = temporary_path / "asar"
        proxy = temporary_path / "codex-mux"

        print("Building multiplexer…")
        build_proxy(proxy)
        print("Copying ChatGPT.app…")
        run(["ditto", str(source), str(staged_app)])
        install_launcher(staged_app)

        resources = staged_app / "Contents" / "Resources"
        original_asar = resources / "app.asar"
        print("Patching desktop profile and renderer…")
        run([str(asar), "extract", str(original_asar), str(extracted)])
        patch_asar_computer_use_identity(extracted, renderer_profile)
        patch_desktop_profile(extracted, installed_computer_use_app)
        patch_renderer(extracted, token, renderer_profile)
        sign_native_code_tree(extracted, signing_identity)
        repacked_asar = temporary_path / "app.asar"
        run(
            [
                str(asar),
                "pack",
                "--unpack-dir",
                ASAR_UNPACK_DIRECTORIES,
                str(extracted),
                str(repacked_asar),
            ]
        )
        asar_listing = output([str(asar), "list", "--is-pack", str(repacked_asar)])
        required_unpacked_module = (
            "unpack : /node_modules/better-sqlite3/build/Release/"
            "better_sqlite3.node"
        )
        if required_unpacked_module not in asar_listing:
            raise RuntimeError("native ASAR modules were not kept unpacked")
        shutil.copy2(repacked_asar, original_asar)
        repacked_unpacked = temporary_path / "app.asar.unpacked"
        if not repacked_unpacked.is_dir():
            raise RuntimeError("ASAR pack did not produce its unpacked native tree")
        shutil.copytree(
            repacked_unpacked,
            resources / "app.asar.unpacked",
            dirs_exist_ok=True,
        )

        bundled_codex = resources / "codex"
        real_codex = resources / "codex.real"
        if real_codex.exists():
            raise RuntimeError("source app already contains codex.real")
        bundled_codex.rename(real_codex)
        shutil.copy2(proxy, bundled_codex)
        bundled_codex.chmod(0o755)

        patch_info_plist(
            staged_app,
            original_asar,
            team_identifier,
            source_report,
        )
        print(f"Signing independent app copy with {signing_identity}…")
        sign_independent_app(
            staged_app,
            signing_identity,
            team_identifier,
            renderer_profile,
        )
        verify_signed_code(
            staged_app,
            DESKTOP_BUNDLE_IDENTIFIER,
            team_identifier,
        )
        verify_signed_code(
            staged_app / "Contents" / "MacOS" / "ChatGPT",
            OPENAI_DESKTOP_CODE_IDENTIFIER,
            team_identifier,
        )
        bundled_computer_use_app = (
            computer_use_package(staged_app) / "Codex Computer Use.app"
        )
        run(
            [
                "ditto",
                str(bundled_computer_use_app),
                str(staged_computer_use_app),
            ]
        )
        verify_signed_code(
            staged_computer_use_app,
            COMPUTER_USE_BUNDLE_IDENTIFIER,
            team_identifier,
        )
        if not skip_update_support:
            install_update_support(
                destination,
                installed_computer_use_app,
                signing_identity,
                allow_adhoc_signing,
            )

        backup_suffix = time.strftime("%Y%m%d-%H%M%S")
        backup_directory = DEFAULT_STATE_ROOT / "backups" / backup_suffix
        app_backup = backup_directory / destination.name
        helper_backup = backup_directory / installed_computer_use_app.name
        had_app = destination.exists()
        had_helper = installed_computer_use_app.exists()
        if had_app or had_helper:
            backup_directory.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            backup_directory.parent.chmod(0o700)
            backup_directory.mkdir(mode=0o700, parents=True, exist_ok=False)
        try:
            if had_app:
                destination.rename(app_backup)
                print(f"Existing copy moved to {app_backup}")
            if had_helper:
                installed_computer_use_app.rename(helper_backup)
                print(f"Existing Computer Use helper moved to {helper_backup}")
            staged_app.rename(destination)
            staged_computer_use_app.rename(installed_computer_use_app)
        except OSError:
            failed_directory = backup_directory / "failed-install"
            failed_directory.mkdir(mode=0o700, parents=True, exist_ok=True)
            if destination.exists():
                destination.rename(failed_directory / destination.name)
            if installed_computer_use_app.exists():
                installed_computer_use_app.rename(
                    failed_directory / installed_computer_use_app.name
                )
            if app_backup.exists():
                app_backup.rename(destination)
            if helper_backup.exists():
                helper_backup.rename(installed_computer_use_app)
            raise

    if LAUNCH_SERVICES_REGISTER.is_file():
        run(
            [
                str(LAUNCH_SERVICES_REGISTER),
                "-f",
                str(destination),
                str(installed_computer_use_app),
            ]
        )
    retire_stale_cached_computer_use_app()

    print(destination)
    print(installed_computer_use_app)


def main() -> int:
    args = parse_args()
    try:
        if args.check_compatibility:
            report = analyze_source_compatibility(args.source)
            print(json.dumps(report, sort_keys=True))
            return 0
        patch_app(
            args.source,
            args.destination,
            args.force,
            args.allow_adhoc_signing,
            args.allow_untested_source,
            args.allow_signing_team_change,
            args.skip_update_support,
        )
    except (RuntimeError, OSError, subprocess.CalledProcessError) as error:
        print(f"patch failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
