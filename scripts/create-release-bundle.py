#!/usr/bin/env python3

import gzip
import pathlib
import sys
import tarfile


def fail(message: str) -> None:
    raise SystemExit(f"error: {message}")


if len(sys.argv) != 5:
    fail(f"usage: {sys.argv[0]} <source> <archive> <root-name> <source-date-epoch>")

source = pathlib.Path(sys.argv[1])
archive = pathlib.Path(sys.argv[2])
root_name = sys.argv[3]
try:
    epoch = int(sys.argv[4])
except ValueError:
    fail("source date epoch must be an integer")

if not source.is_dir() or source.is_symlink():
    fail("bundle source must be a directory")
if not root_name or "/" in root_name or root_name in {".", ".."}:
    fail("bundle root name must be one path component")
if epoch < 1:
    fail("source date epoch must be positive")

entries = sorted(source.rglob("*"), key=lambda path: path.relative_to(source).as_posix())
for path in entries:
    if path.is_symlink() or not (path.is_dir() or path.is_file()):
        fail(f"bundle input is not a regular file or directory: {path}")

archive.parent.mkdir(parents=True, exist_ok=True)
with archive.open("wb") as raw:
    with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=epoch, compresslevel=9) as compressed:
        with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as bundle:
            root = tarfile.TarInfo(root_name)
            root.type = tarfile.DIRTYPE
            root.mode = 0o755
            root.mtime = epoch
            root.uid = root.gid = 0
            root.uname = root.gname = "root"
            bundle.addfile(root)

            for path in entries:
                relative = path.relative_to(source).as_posix()
                info = tarfile.TarInfo(f"{root_name}/{relative}")
                info.mtime = epoch
                info.uid = info.gid = 0
                info.uname = info.gname = "root"
                if path.is_dir():
                    info.type = tarfile.DIRTYPE
                    info.mode = 0o755
                    bundle.addfile(info)
                    continue
                info.mode = 0o755 if path.stat().st_mode & 0o111 else 0o644
                info.size = path.stat().st_size
                with path.open("rb") as contents:
                    bundle.addfile(info, contents)
