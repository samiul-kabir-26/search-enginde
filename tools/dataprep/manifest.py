"""data/MANIFEST.json: parameters, row counts and sha256 for every generated dataset file."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


def sha256_dir(path: Path) -> tuple[str, int]:
    """Hashes a directory as the sorted list of (relative name, file sha256)."""
    h = hashlib.sha256()
    files = sorted(p for p in path.rglob("*") if p.is_file())
    for p in files:
        h.update(f"{p.relative_to(path).as_posix()}\0{sha256_file(p)}\n".encode())
    return h.hexdigest(), len(files)


def describe(path: Path) -> dict:
    if path.is_dir():
        digest, n = sha256_dir(path)
        return {"sha256": digest, "files": n}
    info: dict = {"sha256": sha256_file(path), "bytes": path.stat().st_size}
    if path.suffix == ".jsonl":
        with open(path, "rb") as f:
            info["rows"] = sum(1 for _ in f)
    return info


def update(data_dir: Path, section: str, params: dict, files: list[Path]) -> None:
    """Records one generation step: its parameters and the files it wrote."""
    path = data_dir / "MANIFEST.json"
    manifest = json.loads(path.read_text()) if path.exists() else {"steps": {}, "files": {}}
    manifest["steps"][section] = params
    for f in files:
        if f.exists():
            manifest["files"][f.relative_to(data_dir).as_posix()] = describe(f)
    path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
