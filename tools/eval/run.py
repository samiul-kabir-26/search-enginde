"""Run the offline eval suites against a running stack and write a report.

    uv run python -m eval.run [--suite image] [--limit N]

Needs search-api and control-api (for the token exchange with data/demo_api_key), or
SEARCH_TOKEN. Writes tools/eval/reports/{date}_v{config_version}.md. The text and ranking suites
arrive in M10.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
from pathlib import Path

from eval import image
from eval.client import Client

REPORTS = Path(__file__).resolve().parent / "reports"


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    ap.add_argument(
        "--suite", choices=["image"], action="append", help="suites to run (default: all)"
    )
    ap.add_argument("--limit", type=int, default=0, help="run at most N queries per suite")
    ap.add_argument("--workers", type=int, default=4)
    args = ap.parse_args()
    suites = args.suite or ["image"]
    client = Client.from_env()

    sections, version = [], 0
    if "image" in suites:
        queries = image.build_queries()
        if args.limit:
            queries = queries[: args.limit]
        results = image.run(client, queries, workers=args.workers)
        summary = image.summarize(results)
        probe = client.image_search((image.DATA / queries[0].image_file).read_bytes(), 1)
        version = probe["config_version"]
        sections.append(image.report_markdown(summary, results, version, args.workers))
        print(json.dumps(summary, indent=2))

    REPORTS.mkdir(parents=True, exist_ok=True)
    path = REPORTS / f"{dt.date.today().isoformat()}_v{version}.md"
    header = f"# Offline eval, {dt.date.today().isoformat()}, demo tenant config v{version}\n\n"
    path.write_text(header + "\n".join(sections))
    print(f"report: {path}")


if __name__ == "__main__":
    main()
