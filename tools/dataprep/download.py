"""Download ABO files over HTTPS with parallel range requests, retries and integrity checks.

The ABO bucket can be slow per connection, so large files are fetched as parallel byte ranges
and the selected images are fetched individually (`select.py` needs ~11K small images, not the
3 GB archive). If `abo-images-small.tar` is already in data/raw/abo, select.py reads from it.

    uv run python -m dataprep.download          # listings archive + images.csv.gz
"""

from __future__ import annotations

import argparse
import concurrent.futures as cf
import gzip
import os
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

BASE = "https://amazon-berkeley-objects.s3.amazonaws.com"
ROOT = Path(__file__).resolve().parents[2]
RAW = ROOT / "data" / "raw" / "abo"

LISTINGS_TAR = "archives/abo-listings.tar"
IMAGES_CSV = "images/metadata/images.csv.gz"
SMALL_IMAGES = "images/small"

USER_AGENT = "search-engine-dataprep/1"


def _request(url: str, headers: dict[str, str] | None = None, timeout: float = 60):
    req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT, **(headers or {})})
    return urllib.request.urlopen(req, timeout=timeout)


def content_length(url: str) -> int:
    req = urllib.request.Request(url, method="HEAD", headers={"User-Agent": USER_AGENT})
    with urllib.request.urlopen(req, timeout=60) as resp:
        return int(resp.headers["Content-Length"])


def _with_retries(fn, what: str, attempts: int = 8):
    delay = 1.0
    for i in range(attempts):
        try:
            return fn()
        except (urllib.error.URLError, TimeoutError, ConnectionError, OSError) as e:
            if isinstance(e, urllib.error.HTTPError) and e.code == 404:
                raise
            if i == attempts - 1:
                raise RuntimeError(f"{what}: giving up after {attempts} attempts: {e}") from e
            time.sleep(delay)
            delay = min(delay * 2, 30)


def fetch_ranges(url: str, dest: Path, parts: int = 32) -> None:
    """Downloads url into dest using `parts` concurrent byte-range requests."""
    if dest.exists():
        return
    size = content_length(url)
    step = -(-size // parts)
    dest.parent.mkdir(parents=True, exist_ok=True)
    tmp = dest.with_suffix(dest.suffix + ".part")
    with open(tmp, "wb") as f:
        f.truncate(size)

    def get(start: int) -> int:
        end = min(start + step, size) - 1

        def once() -> bytes:
            with _request(url, {"Range": f"bytes={start}-{end}"}) as resp:
                data = resp.read()
            if len(data) != end - start + 1:
                raise ConnectionError(f"short read {len(data)} for range {start}-{end}")
            return data

        data = _with_retries(once, f"{url} [{start}-{end}]")
        with open(tmp, "r+b") as f:
            f.seek(start)
            f.write(data)
        return len(data)

    with cf.ThreadPoolExecutor(parts) as pool:
        done = sum(pool.map(get, range(0, size, step)))
    if done != size:
        raise RuntimeError(f"{url}: got {done} of {size} bytes")
    os.replace(tmp, dest)


def fetch_small_images(items: list[tuple[str, Path]], workers: int = 64) -> int:
    """Fetches small images, given as (images.csv.gz `path`, destination file) pairs.

    Returns how many were downloaded (already-present files are skipped).
    """

    def get(item: tuple[str, Path]) -> int:
        path, dest = item
        if dest.exists():
            return 0
        dest.parent.mkdir(parents=True, exist_ok=True)

        def once() -> bytes:
            with _request(f"{BASE}/{SMALL_IMAGES}/{path}") as resp:
                return resp.read()

        data = _with_retries(once, path)
        fd, tmp = tempfile.mkstemp(dir=dest.parent, suffix=".part")
        with os.fdopen(fd, "wb") as f:
            f.write(data)
        os.replace(tmp, dest)
        return 1

    items = list(dict((dest, (path, dest)) for path, dest in items).values())
    fetched = 0
    with cf.ThreadPoolExecutor(workers) as pool:
        for i, n in enumerate(pool.map(get, items), 1):
            fetched += n
            if i % 1000 == 0:
                print(f"  images: {i}/{len(items)}", flush=True)
    return fetched


def ensure_metadata(raw: Path = RAW) -> None:
    """Makes sure the listings and images.csv.gz are present and readable."""
    tar = raw / "abo-listings.tar"
    fetch_ranges(f"{BASE}/{LISTINGS_TAR}", tar, parts=16)
    if not (raw / "listings" / "metadata").is_dir():
        with tarfile.open(tar) as t:
            t.extractall(raw, filter="data")
    csv = raw / IMAGES_CSV
    fetch_ranges(f"{BASE}/{IMAGES_CSV}", csv, parts=32)
    with gzip.open(csv, "rt") as f:
        if f.readline().strip() != "image_id,height,width,path":
            raise RuntimeError(f"{csv}: unexpected header")
    for name in ("LICENSE-CC-BY-4.0.txt", "README.md"):
        if not (raw / name).exists():
            fetch_ranges(f"{BASE}/{name}", raw / name, parts=1)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    ap.add_argument("--raw", type=Path, default=RAW)
    args = ap.parse_args()
    ensure_metadata(args.raw)
    print(f"ABO metadata ready in {args.raw}")


if __name__ == "__main__":
    main()
