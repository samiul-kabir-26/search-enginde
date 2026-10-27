"""Run the offline eval suites against a running stack and write a report.

    uv run python -m eval.run [--suite text|image|ranking ...] [--limit N]

Needs search-api and control-api (for the token exchange with data/demo_api_key), or
SEARCH_TOKEN. Each suite warms the stack up first: the first queries after a restart can
exceed their stage budgets (PLAN.md D38). Writes tools/eval/reports/{date}_v{config_version}.md.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
from pathlib import Path

from eval import image, ranking, text
from eval.client import Client

REPORTS = Path(__file__).resolve().parent / "reports"
SUITES = ["text", "image", "ranking"]
WARM_UP = [
    "office chair",
    "red leather handbag",
    "lamp",
    "gold earrings",
    "mug",
    "sofa",
    "water bottle",
    "rug",
]


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    ap.add_argument("--suite", choices=SUITES, action="append", help="suites to run (default: all)")
    ap.add_argument("--limit", type=int, default=0, help="run at most N queries per suite")
    ap.add_argument(
        "--ranking-limit",
        type=int,
        default=600,
        help="held-out sessions for the ranking suite (0: all)",
    )
    ap.add_argument("--workers", type=int, default=4)
    ap.add_argument(
        "--version",
        type=int,
        default=0,
        help="evaluate this published config version through the playground proxy instead of the\n"
        "active one (needs PLATFORM_ADMIN_EMAIL and PLATFORM_ADMIN_PASSWORD; nothing is activated)",
    )
    args = ap.parse_args()
    suites = args.suite or SUITES
    client = Client.pinned(args.version) if args.version else Client.from_env()
    client.warm_up(WARM_UP)

    versions: set[int] = set()
    sections, summaries = [], {}

    if "text" in suites:
        queries = text.build_queries()
        if args.limit:
            queries = queries[: args.limit]
        results = text.run(client, queries, workers=args.workers)
        summaries["text"] = text.summarize(results)
        version = client.text_search(queries[0].text, 1)["config_version"]
        versions.add(version)
        sections.append(text.report_markdown(summaries["text"], results, version, args.workers))

    if "image" in suites:
        queries = image.build_queries()
        if args.limit:
            queries = queries[: args.limit]
        results = image.run(client, queries, workers=args.workers)
        summaries["image"] = image.summarize(results)
        version = client.image_search((image.DATA / queries[0].image_file).read_bytes(), 1)[
            "config_version"
        ]
        versions.add(version)
        sections.append(image.report_markdown(summaries["image"], results, version, args.workers))

    if "ranking" in suites:
        sessions = ranking.held_out_sessions(limit=args.limit or args.ranking_limit)
        util = ranking.Utility()
        results = ranking.run(client, util, sessions, workers=args.workers)
        summaries["ranking"] = ranking.summarize(results)
        version = client.text_search(sessions[0].query, 1)["config_version"]
        versions.add(version)
        sections.append(ranking.report_markdown(summaries["ranking"], version, args.workers))

    print(json.dumps(summaries, indent=2))
    if len(versions) != 1:
        raise SystemExit(
            f"the active config changed during the run (versions {sorted(versions)}); rerun"
        )
    version = versions.pop()
    REPORTS.mkdir(parents=True, exist_ok=True)
    today = dt.date.today().isoformat()
    path = REPORTS / f"{today}_v{version}.md"
    how = (
        "through the playground proxy (not the active version)"
        if args.version
        else "the active version"
    )
    header = f"# Offline eval, {today}, demo tenant config v{version}\n\n"
    header += f"Suites: {', '.join(suites)}; {how}.\n\n"
    path.write_text(header + "\n".join(sections))
    print(f"report: {path}")


if __name__ == "__main__":
    main()
