#!/usr/bin/env python3
"""Deterministic, fail-closed host backup and generation restore for Fern."""

import argparse
import hashlib
import json
import os
import re
import shutil
import stat
import sys
import tarfile
import tempfile
from pathlib import Path, PurePosixPath


SAFE_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
DIR_HASH = hashlib.sha256(b"fern-directory-v1\n").hexdigest()


def fail(message):
    raise RuntimeError(message)


def digest(path):
    value = hashlib.sha256()
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(path, flags)
    mode = os.fstat(descriptor).st_mode
    if not stat.S_ISREG(mode):
        os.close(descriptor)
        fail(f"checksum input is not a regular file: {path}")
    with os.fdopen(descriptor, "rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def valid_id(value, name):
    if not SAFE_ID.fullmatch(value):
        fail(f"{name} must use only letters, digits, dot, underscore, and hyphen")
    return value


class OperatorLock:
    def __init__(self, lock_dir, epoch, initialize=False):
        self.root = Path(lock_dir).resolve(strict=False)
        self.epoch = valid_id(epoch, "epoch")
        self.initialize = initialize
        self.held = self.root / "operator.lock"

    def __enter__(self):
        if self.root.exists() and self.root.is_symlink():
            fail(f"lock directory is a symlink: {self.root}")
        self.root.mkdir(mode=0o700, parents=True, exist_ok=True)
        ensure_private_directory(self.root, "operator lock directory")
        try:
            self.held.mkdir(mode=0o700)
        except FileExistsError:
            fail(f"operator lock is already held: {self.held}")
        try:
            (self.held / "owner").write_text(f"pid={os.getpid()}\n", encoding="ascii")
            marker = self.root / "appliance-epoch"
            if self.initialize:
                try:
                    descriptor = os.open(marker, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                except FileExistsError:
                    fail(f"appliance epoch is already initialized: {marker}")
                with os.fdopen(descriptor, "w", encoding="ascii") as stream:
                    stream.write(self.epoch + "\n")
            if not marker.is_file() or marker.is_symlink():
                fail(f"missing regular appliance epoch marker: {marker}")
            actual = marker.read_text(encoding="ascii").strip()
            if actual != self.epoch:
                fail(f"appliance epoch mismatch: selected {self.epoch}, active {actual}")
        except Exception:
            shutil.rmtree(self.held, ignore_errors=True)
            raise
        return self

    def __exit__(self, _type, _value, _traceback):
        shutil.rmtree(self.held)


def credential_path(label, relative):
    parts = tuple(part.lower() for part in PurePosixPath(relative).parts)
    name = parts[-1] if parts else ""
    if label.startswith("volume ") and label.removeprefix("volume ").endswith("-v1-gh-config"):
        if name in {"hosts.yml", "hosts.yaml"} or "gh" in parts:
            return "workspace-gh"
    if ".config" in parts and "gh" in parts:
        return "workspace-gh"
    if name == "hosts.yml" and "gh" in parts:
        return "workspace-gh"
    if name == "app-credentials.json" and "github-app" in parts:
        return "github-app"
    if len(parts) >= 2 and parts[-2:] == (".git", "config"):
        return "application"
    if label == "config" and (name.endswith(".env") or name == ".env"):
        return "application"
    if name in {".env", "credentials", "credentials.json", "auth.json"}:
        return "application"
    if name.endswith((".pem", ".key")):
        return "application"
    return None


def scan_tree(label, root):
    root = Path(root).resolve(strict=True)
    if not root.exists() or not root.is_dir() or root.is_symlink():
        fail(f"{label} source must be an existing, non-symlink directory: {root}")
    regular = []
    directories = []
    credentials = []
    inodes = set()
    for current, dirnames, filenames in os.walk(root, topdown=True, followlinks=False):
        dirnames.sort()
        filenames.sort()
        current_path = Path(current)
        for name in list(dirnames):
            path = current_path / name
            details = path.lstat()
            mode = details.st_mode
            if stat.S_ISLNK(mode):
                fail(f"symlink rejected in {label}: {path}")
            if not stat.S_ISDIR(mode):
                fail(f"special entry rejected in {label}: {path}")
            directories.append((path.relative_to(root).as_posix(), path))
        for name in filenames:
            path = current_path / name
            details = path.lstat()
            mode = details.st_mode
            if stat.S_ISLNK(mode):
                fail(f"symlink rejected in {label}: {path}")
            if not stat.S_ISREG(mode):
                fail(f"special entry rejected in {label}: {path}")
            identity = (details.st_dev, details.st_ino)
            if details.st_nlink != 1 or identity in inodes:
                fail(f"hard-linked file rejected in {label}: {path}")
            inodes.add(identity)
            relative = path.relative_to(root).as_posix()
            kind = credential_path(label, relative)
            (credentials if kind else regular).append((relative, path, kind))
    return root, directories, regular, credentials


def ensure_private_directory(path, description):
    path = Path(path).resolve(strict=True)
    details = path.stat()
    if not path.is_dir() or path.is_symlink() or details.st_uid != os.geteuid() or details.st_mode & 0o022:
        fail(f"{description} must be an owner-controlled non-symlink directory: {path}")
    return path


def archive_entries(files, directories=()):
    result = {}
    for relative, path in directories:
        result[relative] = ("directory", path, None)
    for item in files:
        relative, path = item[:2]
        result[relative] = ("file", path, digest(path))
        parent = PurePosixPath(relative).parent
        while str(parent) != ".":
            result.setdefault(str(parent), ("directory", None, None))
            parent = parent.parent
    return result


def write_tar(path, entries):
    inventory = []
    with tarfile.open(path, "w", format=tarfile.PAX_FORMAT) as archive:
        for relative in sorted(entries):
            kind, source, sha256 = entries[relative]
            info = tarfile.TarInfo(relative)
            info.uid = info.gid = 0
            info.uname = info.gname = ""
            info.mtime = 0
            if kind == "directory":
                info.type = tarfile.DIRTYPE
                info.mode = 0o755 if source is None else stat.S_IMODE(source.stat().st_mode)
                archive.addfile(info)
                inventory.append({"path": relative, "type": kind, "sha256": DIR_HASH})
            else:
                info.size = source.stat().st_size
                info.mode = stat.S_IMODE(source.stat().st_mode)
                descriptor = os.open(source, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
                opened = os.fstat(descriptor)
                source_details = source.lstat()
                if not stat.S_ISREG(opened.st_mode) or opened.st_nlink != 1 or (opened.st_dev, opened.st_ino) != (source_details.st_dev, source_details.st_ino):
                    os.close(descriptor)
                    fail(f"archive source changed during backup: {source}")
                with os.fdopen(descriptor, "rb") as stream:
                    archive.addfile(info, stream)
                inventory.append({"path": relative, "type": kind, "sha256": sha256})
    fsync_file(path)
    return inventory


def write_json(path, value):
    path.write_text(json.dumps(value, sort_keys=True, indent=2) + "\n", encoding="utf-8")
    fsync_file(path)


def write_sums(root, names):
    lines = [f"{digest(root / name)}  {name}\n" for name in sorted(names)]
    (root / "SHA256SUMS").write_text("".join(lines), encoding="ascii")
    fsync_file(root / "SHA256SUMS")


def fsync_file(path):
    descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def fsync_directory(path):
    descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0))
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def ensure_outside_sources(path, sources, description):
    candidate = Path(path).resolve(strict=False)
    for source in sources:
        try:
            candidate.relative_to(source)
        except ValueError:
            continue
        fail(f"{description} must be outside source tree {source}")


def backup(args):
    generation = valid_id(args.generation, "generation")
    output = Path(args.output).resolve(strict=False)
    if output.exists() or output.is_symlink():
        fail(f"backup output already exists: {output}")
    output.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    ensure_private_directory(output.parent, "backup output parent")
    scans = [scan_tree(name, value) for name, value in (
        ("state", args.state), ("config", args.config), ("repository", args.repository)
    )]
    roots = [item[0] for item in scans]

    volumes = []
    for specification in args.volume:
        if "=" not in specification:
            fail("volume must be NAME=EXPORTED_DIRECTORY")
        name, raw_path = specification.split("=", 1)
        valid_id(name, "volume name")
        if any(existing[0] == name for existing in volumes):
            fail(f"duplicate volume: {name}")
        scanned = scan_tree(f"volume {name}", raw_path)
        roots.append(scanned[0])
        volumes.append((name, scanned))
    ensure_outside_sources(output, roots, "backup output")
    ensure_outside_sources(args.lock_dir, roots, "operator lock directory")
    if volumes and args.credential_policy != "external":
        fail("named volume exports require external credential handling because their contents are opaque")

    external_path = Path(args.credential_output).resolve(strict=False) if args.credential_output else None
    if args.credential_policy == "external" and external_path is None:
        fail("--credential-output is required for external credential handling")
    if args.credential_policy == "exclude" and external_path is not None:
        fail("--credential-output is valid only with external credential handling")
    if external_path:
        if external_path.exists() or external_path.is_symlink() or Path(str(external_path) + ".sha256").exists():
            fail(f"credential recipient output already exists: {external_path}")
        external_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        ensure_private_directory(external_path.parent, "credential recipient parent")
        ensure_outside_sources(external_path, roots, "credential recipient output")
        try:
            external_path.relative_to(output)
        except ValueError:
            pass
        else:
            fail("credential recipient output must be outside the general backup output")

    staging = Path(tempfile.mkdtemp(prefix=f".{output.name}.", dir=output.parent))
    os.chmod(staging, 0o700)
    temp_external = None
    try:
        components = []
        external_files = []
        gh_found = False
        credential_count = 0
        for label, scanned in zip(("state", "config", "repository"), scans):
            _root, directories, files, credentials = scanned
            archive_name = f"{label}.tar"
            inventory = write_tar(staging / archive_name, archive_entries(files, directories))
            components.append({
                "name": label,
                "archive": archive_name,
                "sha256": digest(staging / archive_name),
                "entries": inventory,
            })
            for relative, path, kind in credentials:
                credential_count += 1
                gh_found = gh_found or kind == "workspace-gh"
                if args.credential_policy == "external":
                    external_files.append((f"{label}/{relative}", path, kind))

        volume_names = []
        for name, scanned in volumes:
            _root, directories, files, credentials = scanned
            volume_names.append(name)
            external_files.append((f"volumes/{name}", scanned[0], "volume-directory"))
            for relative, path in directories:
                external_files.append((f"volumes/{name}/{relative}", path, "volume-directory"))
            for relative, path, kind in files + credentials:
                if kind:
                    credential_count += 1
                    gh_found = gh_found or kind == "workspace-gh"
                external_files.append((f"volumes/{name}/{relative}", path, "volume"))

        external = None
        if args.credential_policy == "external":
            descriptor, temporary = tempfile.mkstemp(prefix=f".{external_path.name}.", dir=external_path.parent)
            os.close(descriptor)
            temp_external = Path(temporary)
            entries = {}
            for relative, path, kind in external_files:
                if kind == "volume-directory":
                    entries[relative] = ("directory", path, None)
                else:
                    entries[relative] = ("file", path, digest(path))
            external_inventory = write_tar(temp_external, entries)
            os.chmod(temp_external, 0o600)
            external_sha = digest(temp_external)
            checksum_path = Path(str(external_path) + ".sha256")
            reservation = os.open(external_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            os.close(reservation)
            checksum = os.open(checksum_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(checksum, "w", encoding="ascii") as stream:
                stream.write(f"{external_sha}  {external_path.name}\n")
            os.replace(temp_external, external_path)
            fsync_directory(external_path.parent)
            external = {
                "file": "operator-supplied-external-recipient",
                "sha256": external_sha,
                "entries": external_inventory,
            }

        verification = Path(tempfile.mkdtemp(prefix=".verify-", dir=staging))
        try:
            for component in components:
                destination = verification / component["name"]
                destination.mkdir()
                extract_verified(staging / component["archive"], destination, component["entries"])
            if external:
                extract_verified(external_path, verification, external["entries"], prefix=verification)
        finally:
            shutil.rmtree(verification, ignore_errors=True)

        manifest = {
            "schema_version": 1,
            "generation": generation,
            "source_appliance_epoch": args.epoch,
            "format": "fern-host-backup-v1",
            "components": components,
            "named_volumes": volume_names,
            "credentials": {
                "policy": args.credential_policy,
                "detected_entries": credential_count,
                "workspace_gh": (
                    "included-in-external-recipient" if gh_found and args.credential_policy == "external"
                    else "excluded-reauthorize" if gh_found else "not-found"
                ),
                "general_archive_contains_detected_plaintext_credentials": False,
                "external": external,
            },
        }
        write_json(staging / "BACKUP-MANIFEST.json", manifest)
        write_sums(staging, ["BACKUP-MANIFEST.json"] + [item["archive"] for item in components])
        fsync_directory(staging)
        os.replace(staging, output)
        fsync_directory(output.parent)
    except Exception:
        shutil.rmtree(staging, ignore_errors=True)
        if external_path:
            external_path.unlink(missing_ok=True)
            Path(str(external_path) + ".sha256").unlink(missing_ok=True)
        if temp_external:
            temp_external.unlink(missing_ok=True)
        raise
    print(f"created backup generation {generation}: {output}")


def parse_sums(bundle):
    sums = {}
    checksum = bundle / "SHA256SUMS"
    if not checksum.is_file() or checksum.is_symlink():
        fail("backup has no regular SHA256SUMS")
    for line in checksum.read_text(encoding="ascii").splitlines():
        match = re.fullmatch(r"([0-9a-f]{64})  ([A-Za-z0-9._-]+)", line)
        if not match or match.group(2) in sums:
            fail("malformed SHA256SUMS")
        sums[match.group(2)] = match.group(1)
    children = list(bundle.iterdir())
    if any(path.is_symlink() or not path.is_file() for path in children):
        fail("backup bundle contains a non-regular top-level entry")
    actual = {path.name for path in children if path.name != "SHA256SUMS"}
    if actual != set(sums):
        fail("backup files do not exactly match SHA256SUMS")
    for name, expected in sums.items():
        path = bundle / name
        if path.is_symlink() or digest(path) != expected:
            fail(f"backup checksum mismatch: {name}")
    return sums


def safe_member_name(name):
    path = PurePosixPath(name)
    return bool(name) and not path.is_absolute() and ".." not in path.parts and str(path) == name


def validate_inventory(entries):
    if not isinstance(entries, list):
        fail("archive inventory must be an array")
    result = {}
    for item in entries:
        if not isinstance(item, dict) or set(item) != {"path", "type", "sha256"}:
            fail("malformed archive inventory entry")
        name = item["path"]
        if not isinstance(name, str) or not safe_member_name(name) or name in result:
            fail("unsafe or duplicate archive inventory path")
        if item["type"] not in {"directory", "file"}:
            fail(f"invalid archive inventory type: {name}")
        if not isinstance(item["sha256"], str) or not re.fullmatch(r"[0-9a-f]{64}", item["sha256"]):
            fail(f"invalid archive inventory checksum: {name}")
        if item["type"] == "directory" and item["sha256"] != DIR_HASH:
            fail(f"invalid directory inventory checksum: {name}")
        result[name] = item
    return result


def extract_verified(archive_path, destination, expected_entries, prefix=None):
    expected = validate_inventory(expected_entries)
    seen = set()
    with tarfile.open(archive_path, "r") as archive:
        for member in archive:
            if not safe_member_name(member.name) or member.name in seen:
                fail(f"unsafe or duplicate archive path: {member.name}")
            seen.add(member.name)
            item = expected.get(member.name)
            if item is None:
                fail(f"archive entry is absent from inventory: {member.name}")
            if not (member.isdir() or member.isreg()) or member.issym() or member.islnk():
                fail(f"link or special archive entry rejected: {member.name}")
            if member.isdir() != (item["type"] == "directory"):
                fail(f"archive entry type mismatch: {member.name}")
            relative = PurePosixPath(member.name)
            target = destination / Path(*relative.parts)
            if prefix is not None:
                parts = relative.parts
                if not parts or parts[0] not in {"state", "config", "repository", "volumes"}:
                    fail(f"invalid external recipient entry: {member.name}")
                target = prefix / Path(*parts)
            if member.isdir():
                target.mkdir(mode=member.mode & 0o777, parents=True, exist_ok=True)
                os.chmod(target, member.mode & 0o777)
                continue
            target.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
            source = archive.extractfile(member)
            if source is None:
                fail(f"cannot read archive entry: {member.name}")
            value = hashlib.sha256()
            descriptor = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, member.mode & 0o777)
            with os.fdopen(descriptor, "wb") as output:
                for chunk in iter(lambda: source.read(1024 * 1024), b""):
                    value.update(chunk)
                    output.write(chunk)
            if value.hexdigest() != item["sha256"]:
                fail(f"archive entry checksum mismatch: {member.name}")
    if seen != set(expected):
        fail("archive inventory contains entries absent from archive")


def load_backup(bundle):
    bundle = Path(bundle).resolve(strict=True)
    if not bundle.is_dir() or bundle.is_symlink():
        fail(f"backup must be a non-symlink directory: {bundle}")
    ensure_private_directory(bundle, "backup bundle")
    sums = parse_sums(bundle)
    try:
        manifest = json.loads((bundle / "BACKUP-MANIFEST.json").read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        fail(f"invalid backup manifest: {error}")
    if manifest.get("schema_version") != 1 or manifest.get("format") != "fern-host-backup-v1":
        fail("unsupported backup manifest")
    if set(manifest) != {"schema_version", "generation", "source_appliance_epoch", "format", "components", "named_volumes", "credentials"}:
        fail("backup manifest has unknown or missing properties")
    valid_id(manifest.get("generation", ""), "backup generation")
    valid_id(manifest.get("source_appliance_epoch", ""), "source appliance epoch")
    expected_names = {"BACKUP-MANIFEST.json"}
    components = manifest.get("components", [])
    if not isinstance(components, list) or not all(isinstance(item, dict) for item in components):
        fail("backup components must be an array of objects")
    if [item.get("name") for item in components] != ["state", "config", "repository"]:
        fail("backup must contain state, config, and repository components in canonical order")
    for component in components:
        if set(component) != {"name", "archive", "sha256", "entries"}:
            fail("backup component has unknown or missing properties")
        name = component.get("archive", "")
        if name != component["name"] + ".tar" or not SAFE_ID.fullmatch(name) or sums.get(name) != component.get("sha256"):
            fail("component checksum is inconsistent with backup manifest")
        expected_names.add(name)
        validate_inventory(component.get("entries"))
    if expected_names != set(sums):
        fail("backup manifest does not account for every checksummed file")
    credentials = manifest.get("credentials")
    if not isinstance(credentials, dict) or set(credentials) != {
        "policy", "detected_entries", "workspace_gh", "general_archive_contains_detected_plaintext_credentials", "external"
    } or credentials.get("policy") not in {"exclude", "external"}:
        fail("backup manifest has an invalid credential policy")
    volumes = manifest.get("named_volumes")
    if not isinstance(volumes, list) or len(volumes) != len(set(volumes)) or any(not isinstance(name, str) or not SAFE_ID.fullmatch(name) for name in volumes):
        fail("backup manifest has invalid named volumes")
    if not isinstance(credentials.get("detected_entries"), int) or credentials["detected_entries"] < 0 or credentials.get("general_archive_contains_detected_plaintext_credentials") is not False:
        fail("backup manifest has invalid credential counts or claims")
    if credentials.get("workspace_gh") not in {"included-in-external-recipient", "excluded-reauthorize", "not-found"}:
        fail("backup manifest has invalid workspace gh disposition")
    external = credentials.get("external")
    if credentials["policy"] == "external":
        if not isinstance(external, dict) or set(external) != {"file", "sha256", "entries"} or external.get("file") != "operator-supplied-external-recipient" or not re.fullmatch(r"[0-9a-f]{64}", external.get("sha256", "")):
            fail("backup manifest has invalid external recipient metadata")
        validate_inventory(external.get("entries"))
    elif external is not None:
        fail("excluded credential policy cannot name an external recipient")
    return bundle, manifest


def generation_marker(path, name):
    marker = path / name
    if not marker.is_file() or marker.is_symlink():
        fail(f"generation has no regular {name} marker: {path}")
    return marker.read_text(encoding="ascii").strip()


def verify_generation_epoch(path, epoch):
    if path.exists():
        if not path.is_dir() or path.is_symlink() or generation_marker(path, ".fern-appliance-epoch") != epoch:
            fail(f"generation is fenced by another appliance epoch: {path}")


def activate(target, stage, epoch):
    current = target / "current"
    previous = target / "previous"
    discarded = target / ".discarded-previous"
    for path in (current, previous, discarded):
        if path.is_symlink():
            fail(f"generation path is a symlink: {path}")
    verify_generation_epoch(current, epoch)
    verify_generation_epoch(previous, epoch)
    if discarded.exists():
        fail(f"incomplete earlier activation requires operator review: {discarded}")
    if previous.exists():
        os.replace(previous, discarded)
    try:
        if current.exists():
            os.replace(current, previous)
        os.replace(stage, current)
    except Exception:
        if not current.exists() and previous.exists():
            os.replace(previous, current)
        if discarded.exists() and not previous.exists():
            os.replace(discarded, previous)
        raise
    if discarded.exists():
        shutil.rmtree(discarded)
    fsync_directory(target)


def transaction_manifest(operation, phase, epoch, generation, backup, target):
    current = target / "current"
    previous = target / "previous"
    previous_generation = generation_marker(previous, ".fern-generation") if previous.exists() else None
    manifest = {
        "schema_version": 1,
        "operation_id": f"{operation}-{generation}",
        "operation": operation,
        "phase": phase,
        "appliance_epoch": epoch,
        "generation": generation,
        "backup": {
            "format": "fern-host-backup-v1",
            "manifest": "BACKUP-MANIFEST.json",
            "checksum_file": "SHA256SUMS",
            "credential_policy": backup["credentials"]["policy"],
            "named_volumes": backup["named_volumes"],
        },
        "activation": {
            "model": "staged-current-previous",
            "epoch_marker": ".fern-appliance-epoch",
            "current_generation": generation_marker(current, ".fern-generation"),
        },
        "rollback": {"available": previous_generation is not None, "previous_generation": previous_generation},
        "evidence": {
            "bundle_path": str(Path(backup.get("_bundle_path", "unavailable"))),
            "sha256sums_path": str(Path(backup.get("_bundle_path", "unavailable")) / "SHA256SUMS"),
        },
    }
    manifest_path = target / "TRANSACTION-MANIFEST.json"
    temporary = target / ".transaction-manifest.tmp"
    if temporary.exists() or temporary.is_symlink():
        fail(f"pending transaction manifest requires operator review: {temporary}")
    write_json(temporary, manifest)
    os.replace(temporary, manifest_path)
    fsync_directory(target)


def restore(args):
    bundle, manifest = load_backup(args.backup)
    manifest["_bundle_path"] = str(Path(args.backup).resolve(strict=True))
    target = Path(args.target).resolve(strict=False)
    try:
        target.relative_to(bundle)
    except ValueError:
        pass
    else:
        fail("restore target must be outside the backup bundle")
    if target.exists() and (not target.is_dir() or target.is_symlink()):
        fail(f"restore target must be a non-symlink directory: {target}")
    target.mkdir(mode=0o700, parents=True, exist_ok=True)
    ensure_private_directory(target, "restore target")
    generation = manifest["generation"]
    stage = target / f".staging-{generation}"
    if stage.exists() or stage.is_symlink():
        fail(f"restore staging path already exists: {stage}")
    stage.mkdir(mode=0o700)
    try:
        for component in manifest["components"]:
            destination = stage / component["name"]
            destination.mkdir(mode=0o700)
            extract_verified(bundle / component["archive"], destination, component["entries"])
        external = manifest["credentials"].get("external")
        if external:
            if not args.credential_input:
                fail("this backup requires the external credential/volume recipient")
            credential_input = Path(args.credential_input).absolute()
            if not credential_input.is_file() or credential_input.is_symlink():
                fail("external credential recipient must be a regular file")
            details = credential_input.stat()
            if details.st_uid != os.geteuid() or details.st_mode & 0o077:
                fail("external credential recipient must be owner-controlled mode 0600")
            if digest(credential_input) != external["sha256"]:
                fail("external credential recipient checksum mismatch")
            extract_verified(credential_input, stage, external["entries"], prefix=stage)
        elif args.credential_input:
            fail("backup manifest does not authorize an external credential recipient")
        (stage / ".fern-appliance-epoch").write_text(args.epoch + "\n", encoding="ascii")
        (stage / ".fern-source-epoch").write_text(manifest["source_appliance_epoch"] + "\n", encoding="ascii")
        (stage / ".fern-generation").write_text(generation + "\n", encoding="ascii")
        for marker in (".fern-appliance-epoch", ".fern-source-epoch", ".fern-generation"):
            fsync_file(stage / marker)
        shutil.copyfile(bundle / "BACKUP-MANIFEST.json", stage / "BACKUP-MANIFEST.json")
        fsync_file(stage / "BACKUP-MANIFEST.json")
        fsync_directory(stage)
        activate(target, stage, args.epoch)
        transaction_manifest("restore", "activated", args.epoch, generation, manifest, target)
    except Exception:
        shutil.rmtree(stage, ignore_errors=True)
        raise
    print(f"activated restored generation {generation}: {target / 'current'}")


def rollback(args):
    target = Path(args.target).resolve(strict=True)
    if not target.is_dir() or target.is_symlink():
        fail(f"rollback target must be a non-symlink directory: {target}")
    current = target / "current"
    previous = target / "previous"
    swap = target / ".rollback-swap"
    if swap.exists() or swap.is_symlink() or not current.is_dir() or not previous.is_dir():
        fail("rollback requires exactly current and previous generations with no pending swap")
    for generation in (current, previous):
        if generation.is_symlink():
            fail(f"generation is a symlink: {generation}")
        verify_generation_epoch(generation, args.epoch)
    os.replace(current, swap)
    try:
        os.replace(previous, current)
        os.replace(swap, previous)
    except Exception:
        if not current.exists() and previous.exists():
            os.replace(previous, current)
        if swap.exists() and not previous.exists():
            os.replace(swap, previous)
        raise
    fsync_directory(target)
    generation = generation_marker(current, ".fern-generation")
    try:
        backup = json.loads((current / "BACKUP-MANIFEST.json").read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        fail(f"rolled-back generation has no valid backup manifest: {error}")
    backup["_bundle_path"] = "unavailable-after-rollback"
    transaction_manifest("rollback", "rolled-back", args.epoch, generation, backup, target)
    print(f"rolled back to generation {generation}")


def parser():
    result = argparse.ArgumentParser(description=__doc__)
    commands = result.add_subparsers(dest="command", required=True)
    initialize = commands.add_parser("init-epoch")
    initialize.add_argument("--lock-dir", required=True)
    initialize.add_argument("--epoch", required=True)

    create = commands.add_parser("backup")
    create.add_argument("--lock-dir", required=True)
    create.add_argument("--epoch", required=True)
    create.add_argument("--generation", required=True)
    create.add_argument("--output", required=True)
    create.add_argument("--state", required=True)
    create.add_argument("--config", required=True)
    create.add_argument("--repository", required=True)
    create.add_argument("--volume", action="append", default=[])
    create.add_argument("--credential-policy", choices=("exclude", "external"), required=True)
    create.add_argument("--credential-output")

    recover = commands.add_parser("restore")
    recover.add_argument("--lock-dir", required=True)
    recover.add_argument("--epoch", required=True)
    recover.add_argument("--backup", required=True)
    recover.add_argument("--target", required=True)
    recover.add_argument("--credential-input")

    revert = commands.add_parser("rollback")
    revert.add_argument("--lock-dir", required=True)
    revert.add_argument("--epoch", required=True)
    revert.add_argument("--target", required=True)
    return result


def main():
    args = parser().parse_args()
    try:
        if args.command == "init-epoch":
            with OperatorLock(args.lock_dir, args.epoch, initialize=True):
                pass
            print(f"initialized appliance epoch {args.epoch}: {Path(args.lock_dir).absolute()}")
            return
        with OperatorLock(args.lock_dir, args.epoch):
            if args.command == "backup":
                backup(args)
            elif args.command == "restore":
                restore(args)
            else:
                rollback(args)
    except (RuntimeError, OSError, tarfile.TarError) as error:
        print(f"error: {error}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
