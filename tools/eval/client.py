"""search-api client for the eval suites: token exchange and search calls."""

from __future__ import annotations

import base64
import json
import os
from pathlib import Path

import requests
from requests.adapters import HTTPAdapter
from urllib3.util.retry import Retry

ROOT = Path(__file__).resolve().parents[2]
DATA = ROOT / "data"


class Client:
    def __init__(self, search_url: str, token: str):
        self.search_url = search_url.rstrip("/")
        self.session = requests.Session()
        self.session.headers["Authorization"] = f"Bearer {token}"
        # The plan's rate limit answers 429 with Retry-After; back off instead of failing the run.
        retry = Retry(
            total=8,
            status_forcelist=[429],
            allowed_methods=None,
            respect_retry_after_header=True,
            backoff_factor=0.2,
        )
        self.session.mount("http://", HTTPAdapter(max_retries=retry))
        self.session.mount("https://", HTTPAdapter(max_retries=retry))
        # Set by pinned(): search a given config version through control-api's playground proxy.
        self.playground: tuple[str, int] | None = None

    @classmethod
    def pinned(cls, version: int) -> Client:
        """Searches config `version` of the demo tenant (published, not necessarily active)
        through control-api's playground proxy, signed in as the platform admin
        (PLATFORM_ADMIN_EMAIL, PLATFORM_ADMIN_PASSWORD). Nothing is activated."""
        control = os.environ.get("CONTROL_API_URL", "http://localhost:8081").rstrip("/")
        key = (DATA / "demo_api_key").read_text().strip()
        resp = requests.post(f"{control}/v1/auth/token", json={"api_key": key}, timeout=10)
        resp.raise_for_status()
        claims = resp.json()["access_token"].split(".")[1]
        tid = json.loads(base64.urlsafe_b64decode(claims + "=" * (-len(claims) % 4)))["tid"]
        resp = requests.post(
            f"{control}/v1/admin/login",
            json={
                "email": os.environ["PLATFORM_ADMIN_EMAIL"],
                "password": os.environ["PLATFORM_ADMIN_PASSWORD"],
            },
            timeout=10,
        )
        resp.raise_for_status()
        c = cls(f"{control}/v1/tenants/{tid}/playground", resp.json()["access_token"])
        c.playground = (control, version)
        return c

    def _search_path(self) -> str:
        return f"{self.search_url}/search" if self.playground else f"{self.search_url}/v1/search"

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
        url, params = f"{self._search_path()}/image", None
        if self.playground:
            params = {"config_version": self.playground[1]}
        resp = self.session.post(url, files=files, data=data, params=params, timeout=30)
        resp.raise_for_status()
        return resp.json()

    def text_search(self, q: str, size: int, user_id: str = "") -> dict:
        body: dict = {"q": q, "page": {"size": size}}
        if user_id:
            body["user"] = {"id": user_id}
        if self.playground:
            body["config_version"] = self.playground[1]
        resp = self.session.post(self._search_path(), json=body, timeout=30)
        resp.raise_for_status()
        return resp.json()

    def warm_up(self, queries: list[str], rounds: int = 3) -> None:
        """Opens connections and warms the stores' caches before measuring: the first queries
        after a restart can time out their stage budgets (PLAN.md D38)."""
        for _ in range(rounds):
            for q in queries:
                try:
                    self.text_search(q, 10)
                except requests.RequestException:
                    pass
