"""search-api client for the eval suites: token exchange and search calls."""

from __future__ import annotations

import os
from pathlib import Path

import requests

ROOT = Path(__file__).resolve().parents[2]
DATA = ROOT / "data"


class Client:
    def __init__(self, search_url: str, token: str):
        self.search_url = search_url.rstrip("/")
        self.session = requests.Session()
        self.session.headers["Authorization"] = f"Bearer {token}"

    @classmethod
    def from_env(cls) -> Client:
        """Uses SEARCH_TOKEN, or exchanges data/demo_api_key at CONTROL_API_URL."""
        search = os.environ.get("SEARCH_API_URL", "http://localhost:8080")
        token = os.environ.get("SEARCH_TOKEN")
        if not token:
            control = os.environ.get("CONTROL_API_URL", "http://localhost:8081")
            key = (DATA / "demo_api_key").read_text().strip()
            resp = requests.post(f"{control}/v1/auth/token", json={"api_key": key}, timeout=10)
            resp.raise_for_status()
            token = resp.json()["access_token"]
        return cls(search, token)

    def image_search(self, image: bytes, size: int, q: str = "") -> dict:
        files = {"image": ("query.jpg", image, "image/jpeg")}
        data = {"page": f'{{"size": {size}}}'}
        if q:
            data["q"] = q
        resp = self.session.post(
            f"{self.search_url}/v1/search/image", files=files, data=data, timeout=30
        )
        resp.raise_for_status()
        return resp.json()
