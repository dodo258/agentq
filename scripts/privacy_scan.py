#!/usr/bin/env python3
"""Fail-closed privacy scanner for a Git source tree or directory candidate.

Exit codes: 0 clean, 1 findings, 2 configuration or scan error.
Findings are intentionally limited to ``path:line:type``.
"""

from __future__ import annotations

import argparse
import hashlib
import ipaddress
import json
import os
from pathlib import Path, PurePosixPath
import re
import stat
import subprocess
import sys
from dataclasses import dataclass
from typing import Iterable

MAX_TEXT_BYTES = 16 * 1024 * 1024
DEFAULT_PATTERNS = "~/.agentq/private/privacy-patterns.json"
DEFAULT_MANIFEST = "config/clean-source.json"

MEDIA_EXTENSIONS = {
    ".avif", ".bmp", ".flac", ".gif", ".heic", ".heif", ".ico", ".jpeg",
    ".jpg", ".m4a", ".m4v", ".mkv", ".mov", ".mp3", ".mp4", ".ogg",
    ".otf", ".pdf", ".png", ".svg", ".tif", ".tiff", ".ttf", ".wav",
    ".webm", ".webp", ".woff", ".woff2",
}
ARCHIVE_EXTENSIONS = {
    ".7z", ".bz2", ".dmg", ".gz", ".iso", ".jar", ".pkg", ".rar",
    ".tar", ".tgz", ".txz", ".xz", ".zip",
}

EMAIL_RE = re.compile(
    r"(?<![A-Za-z0-9._%+\-])[A-Za-z0-9._%+\-]+@"
    r"(?:[A-Za-z0-9\-]+\.)+[A-Za-z]{2,63}(?![A-Za-z0-9.\-])"
)
HOME_PATH_RE = re.compile(
    r"(?<![A-Za-z0-9._\-])(?:/(?:Users|home)/[^/\s'\"`]+|"
    + re.escape("/" + "root")
    + r"(?:/[^/\s'\"`]+)?|[A-Za-z]:\\+Users\\+[^\\\s'\"`]+)"
)
IPV4_RE = re.compile(r"(?<![0-9.])(?:[0-9]{1,3}\.){3}[0-9]{1,3}(?![0-9.])")
IPV6_RE = re.compile(
    r"(?<![0-9A-Fa-f:])(?:[0-9A-Fa-f]{0,4}:){2,7}[0-9A-Fa-f]{0,4}"
    r"(?![0-9A-Fa-f:])"
)
CREDENTIAL_RES = (
    (
        "credential-private-key",
        re.compile(r"-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----"),
    ),
    ("credential-aws-access-key", re.compile(r"\b(?:AKIA|ASIA)[0-9A-Z]{16}\b")),
    ("credential-github-token", re.compile(r"\bgh[pousr]_[A-Za-z0-9]{20,}\b")),
    (
        "credential-slack-token",
        re.compile(r"\bxox[baprs]-[A-Za-z0-9-]{20,}\b"),
    ),
    (
        "credential-anthropic-token",
        re.compile(r"\bsk-ant-[A-Za-z0-9_-]{20,}\b"),
    ),
    (
        "credential-generic-token",
        re.compile(r"\bsk-[A-Za-z0-9][A-Za-z0-9_-]{19,}\b"),
    ),
    (
        "credential-jwt",
        re.compile(r"\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\."
                   r"[A-Za-z0-9_-]{10,}\b"),
    ),
)
SECRET_ASSIGNMENT_RE = re.compile(
    r"(?i)(?:\b|_)(?:api[_-]?key|auth[_-]?token|access[_-]?token|client[_-]?secret|"
    r"password|passwd|private[_-]?key|secret[_-]?key|secret|token)\b['\"]?\s*[:=]\s*"
    r"['\"]?([A-Za-z0-9+/=_-]{12,})['\"]?"
)
SAFE_TYPE_RE = re.compile(r"[a-z][a-z0-9-]{0,63}\Z")
SHA256_RE = re.compile(r"[0-9a-f]{64}\Z")


class ScanError(Exception):
    """A sanitized fail-closed error safe to print."""


@dataclass(frozen=True)
class LocalPattern:
    finding_type: str
    literal: str
    case_sensitive: bool


@dataclass(frozen=True)
class Manifest:
    private_paths: tuple[str, ...]
    approved_files: dict[str, str]


@dataclass(frozen=True)
class Candidate:
    rel: str
    kind: str  # file, symlink, gitlink, special
    disk_path: Path | None = None
    oid: str | None = None


@dataclass(frozen=True, order=True)
class Finding:
    rel: str
    line: int
    finding_type: str


def unique_json_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise ScanError("duplicate-json-key")
        result[key] = value
    return result


def parse_json(raw: bytes, error_type: str) -> object:
    try:
        text = raw.decode("utf-8")
        return json.loads(text, object_pairs_hook=unique_json_object)
    except ScanError:
        raise
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError):
        raise ScanError(error_type) from None


def is_within(path: Path, root: Path) -> bool:
    try:
        path.relative_to(root)
        return True
    except ValueError:
        pass

    # APFS commonly treats differently-cased spellings as the same path. Compare
    # existing ancestors by identity so a case alias cannot make an in-repo
    # patterns file appear external.
    current = path
    while True:
        try:
            if os.path.samefile(current, root):
                return True
        except OSError:
            pass
        parent = current.parent
        if parent == current:
            return False
        current = parent


def read_patterns(path_arg: str, root: Path) -> list[LocalPattern]:
    path = Path(os.path.expanduser(path_arg))
    try:
        resolved = path.resolve(strict=True)
    except OSError:
        raise ScanError("patterns-missing") from None
    if is_within(resolved, root):
        raise ScanError("patterns-must-be-external")
    try:
        if not resolved.is_file():
            raise ScanError("patterns-not-regular-file")
        if resolved.stat().st_size > MAX_TEXT_BYTES:
            raise ScanError("patterns-too-large")
        raw = resolved.read_bytes()
    except ScanError:
        raise
    except OSError:
        raise ScanError("patterns-unreadable") from None

    obj = parse_json(raw, "patterns-invalid-json")
    if not isinstance(obj, dict) or set(obj) != {"version", "patterns"}:
        raise ScanError("patterns-invalid-schema")
    if obj["version"] != 1 or isinstance(obj["version"], bool):
        raise ScanError("patterns-invalid-version")
    entries = obj["patterns"]
    if not isinstance(entries, list) or not entries:
        raise ScanError("patterns-empty")

    result: list[LocalPattern] = []
    for entry in entries:
        if not isinstance(entry, dict):
            raise ScanError("patterns-invalid-entry")
        if not set(entry).issubset({"type", "literal", "case_sensitive"}):
            raise ScanError("patterns-invalid-entry")
        if set(entry) < {"type", "literal"}:
            raise ScanError("patterns-invalid-entry")
        label = entry["type"]
        literal = entry["literal"]
        case_sensitive = entry.get("case_sensitive", True)
        if (
            not isinstance(label, str)
            or not SAFE_TYPE_RE.fullmatch(label)
            or not isinstance(literal, str)
            or not literal
            or "\n" in literal
            or "\r" in literal
            or not isinstance(case_sensitive, bool)
        ):
            raise ScanError("patterns-invalid-entry")
        result.append(LocalPattern("local-" + label, literal, case_sensitive))
    return result


def normalize_manifest_path(value: object, *, directory_ok: bool) -> str:
    if not isinstance(value, str) or not value or "\\" in value or "\x00" in value:
        raise ScanError("manifest-invalid-path")
    directory = value.endswith("/")
    if directory and not directory_ok:
        raise ScanError("manifest-invalid-path")
    bare = value[:-1] if directory else value
    path = PurePosixPath(bare)
    if path.is_absolute() or bare in {"", "."} or any(part in {"", ".", ".."} for part in path.parts):
        raise ScanError("manifest-invalid-path")
    normalized = path.as_posix()
    return normalized + ("/" if directory else "")


def parse_manifest(raw: bytes) -> Manifest:
    obj = parse_json(raw, "manifest-invalid-json")
    allowed = {"version", "private_paths", "approved_files"}
    if not isinstance(obj, dict) or not set(obj).issubset(allowed) or "version" not in obj:
        raise ScanError("manifest-invalid-schema")
    if obj["version"] != 1 or isinstance(obj["version"], bool):
        raise ScanError("manifest-invalid-version")

    private_obj = obj.get("private_paths", [])
    approved_obj = obj.get("approved_files", {})
    if not isinstance(private_obj, list) or not isinstance(approved_obj, dict):
        raise ScanError("manifest-invalid-schema")

    private_paths: list[str] = []
    seen_private: set[str] = set()
    for value in private_obj:
        path = normalize_manifest_path(value, directory_ok=True)
        if path == DEFAULT_MANIFEST or path in seen_private:
            raise ScanError("manifest-invalid-private-path")
        seen_private.add(path)
        private_paths.append(path)

    approved_files: dict[str, str] = {}
    for raw_path, raw_hash in approved_obj.items():
        path = normalize_manifest_path(raw_path, directory_ok=False)
        if (
            path == DEFAULT_MANIFEST
            or any(path == p or (p.endswith("/") and path.startswith(p)) for p in private_paths)
            or not isinstance(raw_hash, str)
            or not SHA256_RE.fullmatch(raw_hash)
        ):
            raise ScanError("manifest-invalid-approval")
        approved_files[path] = raw_hash
    return Manifest(tuple(sorted(private_paths)), approved_files)


def git_environment() -> dict[str, str]:
    environment = os.environ.copy()
    environment["GIT_NO_REPLACE_OBJECTS"] = "1"
    return environment


def run_git(root: Path | None, args: list[str]) -> bytes:
    command = ["git"]
    if root is not None:
        command.extend(["-C", os.fspath(root)])
    command.extend(args)
    try:
        proc = subprocess.run(
            command, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
            check=False, env=git_environment(),
        )
    except OSError:
        raise ScanError("git-unavailable") from None
    if proc.returncode != 0:
        raise ScanError("git-command-failed")
    return proc.stdout


def git_root() -> Path:
    raw = run_git(None, ["rev-parse", "--show-toplevel"])
    try:
        value = os.fsdecode(raw.rstrip(b"\n"))
        return Path(value).resolve(strict=True)
    except (OSError, ValueError):
        raise ScanError("git-root-invalid") from None


def parse_git_candidates(root: Path, *, staged: bool) -> list[Candidate]:
    raw = run_git(root, ["ls-files", "--cached", "--stage", "-z"])
    result: list[Candidate] = []
    seen: set[str] = set()
    for record in raw.split(b"\0"):
        if not record:
            continue
        try:
            metadata, raw_path = record.split(b"\t", 1)
            mode, oid, stage = metadata.decode("ascii").split(" ")
        except (ValueError, UnicodeDecodeError):
            raise ScanError("git-index-invalid") from None
        if stage != "0":
            raise ScanError("git-index-unmerged")
        rel = os.fsdecode(raw_path).replace(os.sep, "/")
        if rel in seen:
            raise ScanError("git-index-duplicate-path")
        seen.add(rel)
        if mode == "120000":
            kind = "symlink"
        elif mode == "160000":
            kind = "gitlink"
        elif mode in {"100644", "100755"}:
            kind = "file"
        else:
            kind = "special"
        result.append(
            Candidate(
                rel=rel,
                kind=kind,
                disk_path=None if staged else root / Path(rel),
                oid=oid if staged else None,
            )
        )
    return sorted(result, key=lambda item: item.rel)


def private_path(rel: str, manifest: Manifest) -> bool:
    return any(rel == item or (item.endswith("/") and rel.startswith(item)) for item in manifest.private_paths)


def walk_candidates(root: Path, manifest: Manifest) -> list[Candidate]:
    result: list[Candidate] = []

    def onerror(_: OSError) -> None:
        raise ScanError("directory-read-error")

    for current, dirs, files in os.walk(root, topdown=True, followlinks=False, onerror=onerror):
        current_path = Path(current)
        rel_dir = current_path.relative_to(root).as_posix()
        if rel_dir == ".":
            rel_dir = ""

        kept_dirs: list[str] = []
        for name in dirs:
            rel = f"{rel_dir}/{name}" if rel_dir else name
            if name == ".git" or private_path(rel + "/", manifest) or private_path(rel, manifest):
                continue
            full = current_path / name
            try:
                mode = full.lstat().st_mode
            except OSError:
                raise ScanError("path-stat-error") from None
            if stat.S_ISLNK(mode):
                result.append(Candidate(rel, "symlink", full))
            elif stat.S_ISDIR(mode):
                kept_dirs.append(name)
            else:
                result.append(Candidate(rel, "special", full))
        dirs[:] = kept_dirs

        for name in files:
            rel = f"{rel_dir}/{name}" if rel_dir else name
            if name == ".git" or private_path(rel, manifest):
                continue
            full = current_path / name
            try:
                mode = full.lstat().st_mode
            except OSError:
                raise ScanError("path-stat-error") from None
            if stat.S_ISLNK(mode):
                kind = "symlink"
            elif stat.S_ISREG(mode):
                kind = "file"
            else:
                kind = "special"
            result.append(Candidate(rel, kind, full))
    return sorted(result, key=lambda item: item.rel)


def disk_manifest_path(root: Path, value: str) -> tuple[Path, str]:
    path = Path(value)
    if not path.is_absolute():
        path = root / path
    try:
        if path.is_symlink():
            raise ScanError("manifest-must-be-repository-file")
        resolved = path.resolve(strict=True)
    except ScanError:
        raise
    except OSError:
        raise ScanError("manifest-missing") from None
    if not is_within(resolved, root) or not resolved.is_file():
        raise ScanError("manifest-must-be-repository-file")
    rel = resolved.relative_to(root).as_posix()
    return resolved, rel


def manifest_for_disk(root: Path, explicit: str | None) -> tuple[Manifest, str | None]:
    if explicit is not None:
        path, rel = disk_manifest_path(root, explicit)
    else:
        path = root / DEFAULT_MANIFEST
        if not os.path.lexists(path):
            return Manifest((), {}), None
        path, rel = disk_manifest_path(root, os.fspath(path))
    try:
        if path.stat().st_size > MAX_TEXT_BYTES:
            raise ScanError("manifest-too-large")
        return parse_manifest(path.read_bytes()), rel
    except OSError:
        raise ScanError("manifest-unreadable") from None


def candidate_size(candidate: Candidate, root: Path, *, staged: bool) -> int:
    if staged:
        if candidate.oid is None:
            raise ScanError("git-index-invalid")
        raw = run_git(root, ["cat-file", "-s", candidate.oid])
        try:
            size = int(raw.strip())
        except ValueError:
            raise ScanError("git-object-size-invalid") from None
        if size < 0:
            raise ScanError("git-object-size-invalid")
        return size
    if candidate.disk_path is None:
        raise ScanError("source-path-invalid")
    try:
        mode = candidate.disk_path.lstat().st_mode
    except OSError:
        raise ScanError("source-read-error") from None
    if candidate.kind == "symlink" and not stat.S_ISLNK(mode):
        raise ScanError("source-type-changed")
    if candidate.kind == "file" and not stat.S_ISREG(mode):
        raise ScanError("source-type-changed")
    return candidate.disk_path.lstat().st_size


def candidate_bytes(candidate: Candidate, root: Path, *, staged: bool) -> bytes:
    candidate_size(candidate, root, staged=staged)
    if staged:
        if candidate.oid is None:
            raise ScanError("git-index-invalid")
        return run_git(root, ["cat-file", "blob", candidate.oid])
    if candidate.disk_path is None:
        raise ScanError("source-path-invalid")
    try:
        if candidate.kind == "symlink":
            return os.readlink(candidate.disk_path).encode("utf-8", "surrogateescape")
        return candidate.disk_path.read_bytes()
    except OSError:
        raise ScanError("source-read-error") from None


def candidate_sha256(candidate: Candidate, root: Path, *, staged: bool) -> str:
    candidate_size(candidate, root, staged=staged)
    digest = hashlib.sha256()
    if staged:
        if candidate.oid is None:
            raise ScanError("git-index-invalid")
        command = ["git", "-C", os.fspath(root), "cat-file", "blob", candidate.oid]
        try:
            process = subprocess.Popen(
                command, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                env=git_environment(),
            )
        except OSError:
            raise ScanError("git-unavailable") from None
        if process.stdout is None:
            process.kill()
            raise ScanError("git-command-failed")
        for chunk in iter(lambda: process.stdout.read(1024 * 1024), b""):
            digest.update(chunk)
        if process.wait() != 0:
            raise ScanError("git-command-failed")
    else:
        if candidate.disk_path is None:
            raise ScanError("source-path-invalid")
        try:
            with candidate.disk_path.open("rb") as source:
                for chunk in iter(lambda: source.read(1024 * 1024), b""):
                    digest.update(chunk)
        except OSError:
            raise ScanError("source-read-error") from None
    return digest.hexdigest()


def manifest_for_index(
    root: Path, candidates: list[Candidate], explicit: str | None
) -> tuple[Manifest, str | None]:
    by_path = {candidate.rel: candidate for candidate in candidates}
    if explicit is not None:
        path = Path(explicit)
        if path.is_absolute():
            raise ScanError("staged-manifest-must-be-relative")
        rel = normalize_manifest_path(path.as_posix(), directory_ok=False)
        candidate = by_path.get(rel)
        if candidate is None:
            raise ScanError("manifest-missing-from-index")
    else:
        rel = DEFAULT_MANIFEST
        candidate = by_path.get(rel)
        if candidate is None:
            return Manifest((), {}), None
    if candidate.kind != "file":
        raise ScanError("manifest-not-regular-file")
    if candidate_size(candidate, root, staged=True) > MAX_TEXT_BYTES:
        raise ScanError("manifest-too-large")
    return parse_manifest(candidate_bytes(candidate, root, staged=True)), rel


def looks_like_media(rel: str, data: bytes) -> bool:
    if PurePosixPath(rel).suffix.lower() in MEDIA_EXTENSIONS:
        return True
    signatures = (
        b"\x89PNG\r\n\x1a\n", b"\xff\xd8\xff", b"GIF87a", b"GIF89a", b"%PDF-",
        b"RIFF", b"\x00\x00\x00\x18ftyp", b"\x00\x00\x00\x20ftyp",
    )
    return any(data.startswith(signature) for signature in signatures)


def looks_like_archive(rel: str, data: bytes) -> bool:
    if PurePosixPath(rel).suffix.lower() in ARCHIVE_EXTENSIONS:
        return True
    signatures = (
        b"PK\x03\x04", b"PK\x05\x06", b"PK\x07\x08", b"\x1f\x8b", b"BZh",
        b"\xfd7zXZ\x00", b"7z\xbc\xaf'\x1c", b"Rar!\x1a\x07",
    )
    return any(data.startswith(signature) for signature in signatures)


def public_ip_matches(line: str) -> Iterable[str]:
    for regex in (IPV4_RE, IPV6_RE):
        for match in regex.finditer(line):
            try:
                address = ipaddress.ip_address(match.group(0))
            except ValueError:
                continue
            if address.is_global:
                yield match.group(0)


def generic_findings(line: str) -> set[str]:
    result: set[str] = set()
    if EMAIL_RE.search(line):
        result.add("email")
    if HOME_PATH_RE.search(line):
        result.add("absolute-home-path")
    if next(iter(public_ip_matches(line)), None) is not None:
        result.add("public-ip")
    for finding_type, regex in CREDENTIAL_RES:
        if regex.search(line):
            result.add(finding_type)
    if SECRET_ASSIGNMENT_RE.search(line):
        result.add("credential-secret-assignment")
    return result


def local_findings(line: str, patterns: list[LocalPattern]) -> set[str]:
    result: set[str] = set()
    folded: str | None = None
    for pattern in patterns:
        if pattern.case_sensitive:
            matched = pattern.literal in line
        else:
            if folded is None:
                folded = line.casefold()
            matched = pattern.literal.casefold() in folded
        if matched:
            result.add(pattern.finding_type)
    return result


def line_findings(line: str, patterns: list[LocalPattern]) -> set[str]:
    return generic_findings(line) | local_findings(line, patterns)


def unsafe_path_character(char: str) -> bool:
    codepoint = ord(char)
    return (
        codepoint < 32
        or 0x7F <= codepoint <= 0x9F
        or codepoint in {0x061C, 0x200E, 0x200F}
        or 0x202A <= codepoint <= 0x202E
        or 0x2066 <= codepoint <= 0x2069
        or 0xD800 <= codepoint <= 0xDFFF
    )


def path_findings(rel: str, patterns: list[LocalPattern]) -> set[str]:
    result = line_findings(rel, patterns)
    if ":" in rel or any(unsafe_path_character(char) for char in rel):
        result.add("unsafe-path")
    return result


def symlink_outside(candidate: Candidate, root: Path, target: str, *, staged: bool) -> bool:
    if staged:
        target_path = PurePosixPath(target)
        if target_path.is_absolute():
            return True
        depth = 0
        for part in (PurePosixPath(candidate.rel).parent / target_path).parts:
            if part in {"", "."}:
                continue
            if part == "..":
                if depth == 0:
                    return True
                depth -= 1
            else:
                depth += 1
        return False

    if candidate.disk_path is None:
        return True
    base = candidate.disk_path.parent
    target_path = Path(target)
    try:
        resolved = target_path.resolve(strict=False) if target_path.is_absolute() else (base / target_path).resolve(strict=False)
    except (OSError, RuntimeError):
        return True
    return not is_within(resolved, root)


def binary_bytes(data: bytes) -> bool:
    if b"\x00" in data:
        return True
    return any(byte < 9 or 13 < byte < 32 for byte in data)


def scan_candidate(
    candidate: Candidate,
    root: Path,
    staged: bool,
    manifest: Manifest,
    patterns: list[LocalPattern],
) -> list[Finding]:
    findings = [Finding(candidate.rel, 1, item) for item in path_findings(candidate.rel, patterns)]
    if candidate.kind == "gitlink":
        findings.append(Finding(candidate.rel, 1, "unreviewed-gitlink"))
        return findings
    if candidate.kind == "special":
        findings.append(Finding(candidate.rel, 1, "unsupported-file-type"))
        return findings

    if candidate.kind == "symlink":
        data = candidate_bytes(candidate, root, staged=staged)
        try:
            target = data.decode("utf-8")
        except UnicodeDecodeError:
            findings.append(Finding(candidate.rel, 1, "invalid-symlink-target"))
            return findings
        for finding_type in line_findings(target, patterns):
            findings.append(Finding(candidate.rel, 1, finding_type))
        if symlink_outside(candidate, root, target, staged=staged):
            findings.append(Finding(candidate.rel, 1, "external-symlink"))
        return findings

    approved_hash = manifest.approved_files.get(candidate.rel)
    suffix = PurePosixPath(candidate.rel).suffix.lower()
    artifact_type: str | None = None
    if suffix in ARCHIVE_EXTENSIONS:
        artifact_type = "archive"
    elif suffix in MEDIA_EXTENSIONS:
        artifact_type = "media"
    if artifact_type is not None:
        if approved_hash is None:
            findings.append(Finding(candidate.rel, 1, "unreviewed-" + artifact_type))
        elif candidate_sha256(candidate, root, staged=staged) != approved_hash:
            findings.append(Finding(candidate.rel, 1, "approved-file-hash-mismatch"))
        return findings

    size = candidate_size(candidate, root, staged=staged)
    if size > MAX_TEXT_BYTES:
        findings.append(Finding(candidate.rel, 1, "unreviewed-large-file"))
        return findings
    data = candidate_bytes(candidate, root, staged=staged)
    if looks_like_archive(candidate.rel, data[:64]):
        artifact_type = "archive"
    elif looks_like_media(candidate.rel, data[:64]):
        artifact_type = "media"
    if artifact_type is not None:
        if approved_hash is None:
            findings.append(Finding(candidate.rel, 1, "unreviewed-" + artifact_type))
        elif hashlib.sha256(data).hexdigest() != approved_hash:
            findings.append(Finding(candidate.rel, 1, "approved-file-hash-mismatch"))
        return findings
    if binary_bytes(data):
        if approved_hash is None:
            findings.append(Finding(candidate.rel, 1, "unreviewed-binary"))
        elif hashlib.sha256(data).hexdigest() != approved_hash:
            findings.append(Finding(candidate.rel, 1, "approved-file-hash-mismatch"))
        return findings
    try:
        text = data.decode("utf-8-sig")
    except UnicodeDecodeError:
        if approved_hash is None:
            findings.append(Finding(candidate.rel, 1, "unreviewed-binary"))
        elif hashlib.sha256(data).hexdigest() != approved_hash:
            findings.append(Finding(candidate.rel, 1, "approved-file-hash-mismatch"))
        return findings

    # Hash approvals are only for opaque artifacts and never suppress ordinary text.
    if approved_hash is not None:
        findings.append(Finding(candidate.rel, 1, "approval-for-text-file"))
    for line_number, line in enumerate(re.split(r"\r\n|\n|\r", text), 1):
        for finding_type in line_findings(line, patterns):
            findings.append(Finding(candidate.rel, line_number, finding_type))
    return findings


def scan(args: argparse.Namespace) -> tuple[list[Finding], set[str]]:
    staged = bool(args.staged)
    if args.root is not None:
        try:
            root = Path(args.root).resolve(strict=True)
        except OSError:
            raise ScanError("root-missing") from None
        if not root.is_dir():
            raise ScanError("root-not-directory")
    else:
        root = git_root()

    patterns = read_patterns(args.patterns, root)
    manifest_path: str | None
    if staged:
        candidates = parse_git_candidates(root, staged=True)
        manifest, manifest_path = manifest_for_index(root, candidates, args.manifest)
    else:
        manifest, manifest_path = manifest_for_disk(root, args.manifest)
        if args.root is not None:
            candidates = walk_candidates(root, manifest)
        else:
            candidates = parse_git_candidates(root, staged=False)

    if manifest_path is not None and private_path(manifest_path, manifest):
        raise ScanError("manifest-cannot-be-private")

    findings: list[Finding] = []
    sensitive_paths: set[str] = set()
    for candidate in candidates:
        if private_path(candidate.rel, manifest):
            continue
        if path_findings(candidate.rel, patterns):
            sensitive_paths.add(candidate.rel)
        findings.extend(scan_candidate(candidate, root, staged, manifest, patterns))

    # An explicit manifest is itself always part of the public boundary. The path
    # check above catches a sensitive filename; schema validation catches attempts
    # to classify it as private.
    if manifest_path is not None and manifest_path not in {candidate.rel for candidate in candidates}:
        raise ScanError("manifest-outside-scan")
    return sorted(set(findings)), sensitive_paths


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(
        description=(
            "Scan tracked working files (default), the complete staged index, or every "
            "directory candidate except .git. Personal literals come only from an external JSON file."
        )
    )
    modes = result.add_mutually_exclusive_group()
    modes.add_argument("--staged", action="store_true", help="scan blobs and symlinks from the Git index")
    modes.add_argument("--root", metavar="DIRECTORY", help="scan all files under a candidate directory")
    result.add_argument(
        "--patterns",
        default=DEFAULT_PATTERNS,
        metavar="FILE",
        help="external pattern JSON (default: ~/.agentq/private/privacy-patterns.json)",
    )
    result.add_argument(
        "--manifest",
        metavar="FILE",
        help=(
            "clean-source JSON; config/clean-source.json is loaded automatically when present. "
            "Schema: version, private_paths, approved_files (path to SHA-256)"
        ),
    )
    return result


def main() -> int:
    args = parser().parse_args()
    try:
        findings, sensitive_paths = scan(args)
    except ScanError as error:
        print(f"privacy-scan:error:{error}", file=sys.stderr)
        return 2
    except Exception:
        # Never let a traceback disclose a sensitive path or matched source line.
        print("privacy-scan:error:unexpected-scan-error", file=sys.stderr)
        return 2
    if not findings:
        return 0

    # Sensitive filenames are replaced by stable per-run identifiers.
    numbered = {rel: f"file-{index:04d}" for index, rel in enumerate(sorted(sensitive_paths), 1)}
    for finding in findings:
        display = numbered.get(finding.rel, finding.rel)
        print(f"{display}:{finding.line}:{finding.finding_type}")
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
