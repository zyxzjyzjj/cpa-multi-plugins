#!/usr/bin/env python3
"""Package CPA store assets: one root-level library per zip, plus a manual bundle."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import zipfile

ROOT = Path(__file__).resolve().parents[1]


def package(version, target_os, arch, build_dir, output_dir):
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version):
        raise ValueError("version must be numeric X.Y.Z (without v)")
    ext = {"linux": "so", "darwin": "dylib", "windows": "dll"}[target_os]
    registry = json.loads((ROOT / "registry.json").read_text(encoding="utf-8"))
    libraries = [build_dir / f"{p['id']}.{ext}" for p in registry["plugins"]]
    for library in libraries:
        if not library.is_file():
            raise FileNotFoundError(library)
    output_dir.mkdir(parents=True, exist_ok=True)
    archives = []
    for library in libraries:
        archive = output_dir / f"{library.stem}_{version}_{target_os}_{arch}.zip"
        with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as bundle:
            bundle.write(library, library.name)
        archives.append(archive)
    manual = output_dir / f"cpa-multi-plugins-{target_os}-{arch}.zip"
    with zipfile.ZipFile(manual, "w", zipfile.ZIP_DEFLATED) as bundle:
        for library in libraries:
            bundle.write(library, library.name)
    archives.append(manual)
    checksums = "".join(
        f"{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n"
        for p in archives
    )
    (output_dir / f"checksums-{target_os}-{arch}.txt").write_text(checksums, encoding="utf-8")
    return archives


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("version")
    parser.add_argument("os", choices=["linux", "darwin", "windows"])
    parser.add_argument("arch", choices=["amd64", "arm64"])
    parser.add_argument("--output", type=Path, default=ROOT / "release-assets")
    args = parser.parse_args()
    for archive in package(args.version, args.os, args.arch,
                           ROOT / "dist" / f"{args.os}-{args.arch}", args.output):
        print(archive.name)
