"""Trains the platform's learned rankers (docs/MODELS.md §5) and reports them against
default_linear on the held-out split.

    uv run python -m training.train --data ../data/training/demo.csv.gz

The dataset comes from `featurejob dataset` (Go): one row per impression of a logged request,
with the features the serving pipeline computes, the graded label (order 3, cart 2, click 1)
and the time split (80/10/10). Writes, per model, out/<id>/model.onnx and out/<id>/features.json
(the ordered feature schema, sha256 and metrics), and a markdown report.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import time
from pathlib import Path

import lightgbm as lgb
import numpy as np
import onnx
import onnxruntime as ort
import pandas as pd

from eval.metrics import ndcg_at
from training import export

ROOT = Path(__file__).resolve().parents[2]

# Pre-rank: at most 15 cheap features (docs/MODELS.md §5). Online item and vendor features are
# read before S4 scores, so they cost nothing extra.
PRERANK_FEATURES = [
    "rrf_score",
    "bm25_norm",
    "cosine_text",
    "cosine_image",
    "retriever_count",
    "price_minor_log",
    "rating",
    "rating_count_log",
    "listing_completeness",
    "image_count",
    "age_days",
    "query_category_match",
    "ctr_7d",
    "cvr_7d",
    "vendor_score",
]
# policy_boost is added to a learned model's output at serving time (tenant rules change
# without retraining), so no model takes it; user features are re-rank only.
NON_FEATURES = {"policy_boost"}

# Pre-rank scores up to ~2,000 candidates in-process within 5 ms (docs/MODELS.md §5): 120 trees
# of 15 leaves take ~4.5 ms on one thread here, so the tree count is capped below the spec's 200.
# The learning rate is picked from the grid by validation nDCG@10 (early stopping also watches
# validation); the test split is only ever used for the report.
MODELS = {
    "prerank-gbdt-v1": {
        "type": "onnx",
        "baseline": "default_linear (prerank)",
        "max_trees": 120,
        "num_leaves": 15,
        "learning_rates": [0.05, 0.1, 0.15, 0.2],
    },
    "rank-gbdt-v1": {
        "type": "kserve_v2",
        "baseline": "default_linear (rank)",
        "max_trees": 500,
        "num_leaves": 31,
        "learning_rates": [0.05, 0.1],
    },
}
# Columns that are not model features (identifiers, label, baselines).
META = {"request_id", "ts", "split", "product_id", "position", "label", "randomized"}


def load(path: Path) -> pd.DataFrame:
    df = pd.read_csv(path, dtype={"request_id": str, "product_id": str, "split": str})
    # Rows of one request are contiguous (the builder writes them that way); keep them so.
    return df.sort_values(["ts", "request_id", "position"], kind="stable").reset_index(drop=True)


def groups(df: pd.DataFrame) -> np.ndarray:
    return df.groupby("request_id", sort=False).size().to_numpy()


def train_model(
    df: pd.DataFrame,
    features: list[str],
    max_trees: int,
    num_leaves: int,
    seed: int,
    learning_rate: float = 0.05,
) -> tuple[lgb.Booster, dict]:
    tr, va = df[df.split == "train"], df[df.split == "valid"]
    params = {
        "objective": "lambdarank",
        "metric": "ndcg",
        "eval_at": [10],
        "lambdarank_truncation_level": 20,
        "num_leaves": num_leaves,
        "learning_rate": learning_rate,
        "min_data_in_leaf": 100,
        "feature_fraction": 0.9,
        "bagging_fraction": 0.8,
        "bagging_freq": 1,
        "lambda_l2": 1.0,
        "deterministic": True,
        "force_row_wise": True,
        "num_threads": 4,
        "seed": seed,
        "verbose": -1,
    }
    dtrain = lgb.Dataset(
        tr[features].to_numpy(np.float32), tr.label, group=groups(tr), free_raw_data=False
    )
    dvalid = lgb.Dataset(
        va[features].to_numpy(np.float32), va.label, group=groups(va), reference=dtrain
    )
    evals: dict = {}
    booster = lgb.train(
        params,
        dtrain,
        num_boost_round=max_trees,
        valid_sets=[dvalid],
        valid_names=["valid"],
        callbacks=[lgb.early_stopping(50, verbose=False), lgb.record_evaluation(evals)],
    )
    trees = booster.best_iteration or booster.current_iteration()
    return booster, {
        "trees": trees,
        "learning_rate": learning_rate,
        "valid_ndcg10": evals["valid"]["ndcg@10"][trees - 1],
    }


def select(
    df: pd.DataFrame, features: list[str], spec: dict, seed: int
) -> tuple[lgb.Booster, dict]:
    """Trains one model per learning rate and keeps the best by validation nDCG@10."""
    best = None
    for lr in spec["learning_rates"]:
        booster, info = train_model(df, features, spec["max_trees"], spec["num_leaves"], seed, lr)
        print(
            f"  lr {lr}: {info['trees']} trees, valid nDCG@10 {info['valid_ndcg10']:.4f}",
            flush=True,
        )
        if best is None or info["valid_ndcg10"] > best[1]["valid_ndcg10"]:
            best = (booster, info)
    return best


def per_request(df: pd.DataFrame, score_col: str, eta: float | None = None) -> pd.DataFrame:
    """nDCG@10 and reciprocal rank of each request when its impressions are ordered by score_col
    (higher first; ties by logged position). With eta, each gain is divided by the examination
    propensity of its logged position, pos^-eta (inverse propensity scoring, weights capped at
    MAX_IPS_WEIGHT), which removes the advantage position bias gives the logged order."""
    out = []
    for rid, g in df.groupby("request_id", sort=False):
        g = g.sort_values([score_col, "position"], ascending=[False, True], kind="stable")
        rel = g.label.to_numpy() > 0
        rr = 1.0 / (int(np.argmax(rel)) + 1) if rel.any() else 0.0
        if eta is None:
            grades = dict(zip(g.product_id, g.label, strict=True))
            nd = ndcg_at(g.product_id.tolist(), grades, 10)
        else:
            gains = (2.0 ** g.label.to_numpy() - 1) * np.minimum(
                g.position.to_numpy() ** eta, MAX_IPS_WEIGHT
            )
            nd = dcg(gains[:10]) / dcg(np.sort(gains)[::-1][:10]) if gains.any() else 0.0
        out.append({"request_id": rid, "ndcg10": nd, "rr": rr})
    return pd.DataFrame(out)


MAX_IPS_WEIGHT = 10.0


def dcg(gains: np.ndarray) -> float:
    return float(np.sum(gains / np.log2(np.arange(len(gains)) + 2)))


def fit_eta(df: pd.DataFrame) -> float:
    """The examination exponent eta of p(pos) = pos^-eta, fitted like featurejob does
    (internal/featurejob FitPropensity): ln CTR_k = c - eta ln k over randomized impressions,
    weighted by clicks. In a randomized request relevance is independent of position, so the
    CTR at a position measures how often it is examined."""
    r = df[df.randomized]
    per = r.groupby("position").agg(n=("label", "size"), c=("label", lambda x: int((x > 0).sum())))
    per = per[(per.n >= 50) & (per.c > 0)]
    if len(per) < 5 or per.c.sum() < 200:
        return 0.0  # too little randomized traffic: no correction (featurejob does the same)
    x, y, w = np.log(per.index.to_numpy(float)), np.log(per.c / per.n), per.c.to_numpy(float)
    mx, my = np.average(x, weights=w), np.average(y, weights=w)
    slope = np.sum(w * (x - mx) * (y - my)) / np.sum(w * (x - mx) ** 2)
    return float(min(max(-slope, 0.0), 2.0))


def bootstrap_diff(a: np.ndarray, b: np.ndarray, seed: int, n: int = 1000) -> tuple[float, float]:
    """95% bootstrap interval of mean(a - b) over requests."""
    rng = np.random.default_rng(seed)
    d = a - b
    means = [d[rng.integers(0, len(d), len(d))].mean() for _ in range(n)]
    return float(np.percentile(means, 2.5)), float(np.percentile(means, 97.5))


def time_inference(model: onnx.ModelProto, n_features: int, rows: int = 2000) -> float:
    """Median ms to score `rows` candidates in-process with one intra-op thread."""
    so = ort.SessionOptions()
    so.intra_op_num_threads, so.inter_op_num_threads = 1, 1
    sess = ort.InferenceSession(model.SerializeToString(), so, providers=["CPUExecutionProvider"])
    x = np.random.default_rng(0).random((rows, n_features), dtype=np.float32)
    times = []
    for _ in range(30):
        t0 = time.perf_counter()
        sess.run([export.OUTPUT], {export.INPUT: x})
        times.append((time.perf_counter() - t0) * 1000)
    return float(np.median(times))


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main() -> None:
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    ap.add_argument("--data", type=Path, default=ROOT / "data/training/demo.csv.gz")
    ap.add_argument("--out", type=Path, default=ROOT / "tools/training/out")
    ap.add_argument("--reports", type=Path, default=ROOT / "tools/eval/reports")
    ap.add_argument("--seed", type=int, default=42)
    args = ap.parse_args()

    t0 = time.time()
    df = load(args.data)
    all_features = [
        c
        for c in df.columns
        if c not in META and not c.startswith("baseline") and c not in NON_FEATURES
    ]
    test = df[df.split == "test"].copy()
    summary = {
        "rows": len(df),
        "requests": {
            s: int(df[df.split == s].request_id.nunique()) for s in ("train", "valid", "test")
        },
        "positives": float((df.label > 0).mean()),
    }
    print(f"dataset {args.data}: {summary}")

    results = {}
    for model_id, spec in MODELS.items():
        features = PRERANK_FEATURES if model_id.startswith("prerank") else all_features
        print(f"{model_id}: {len(features)} features")
        booster, info = select(df, features, spec, args.seed)
        x_test = test[features].to_numpy(np.float32)
        test[model_id] = booster.predict(x_test, num_iteration=info["trees"])
        model = export.to_onnx(booster_at(booster, info["trees"]), model_id)
        parity = export.check_parity(booster_at(booster, info["trees"]), model, x_test[:5000])
        importance = sorted(
            zip(features, booster.feature_importance("gain", iteration=info["trees"]), strict=True),
            key=lambda kv: -kv[1],
        )
        out_dir = args.out / model_id
        out_dir.mkdir(parents=True, exist_ok=True)
        onnx.save(model, out_dir / "model.onnx")
        results[model_id] = {
            **info,
            "parity": parity,
            "infer_ms_2000": time_inference(model, len(features)),
            "importance": importance[:8],
            "features": features,
            "sha256": sha256(out_dir / "model.onnx"),
            "dir": out_dir,
            "type": spec["type"],
        }

    rows = {
        "logged order": "neg_position",
        "default_linear (prerank)": "baseline_prerank",
        "default_linear (rank)": "baseline_rank",
        "prerank-gbdt-v1": "prerank-gbdt-v1",
        "rank-gbdt-v1": "rank-gbdt-v1",
    }
    test["neg_position"] = -test.position
    # All test requests as logged (position-biased: users mostly clicked what was shown first),
    # and inverse propensity weighted, which corrects for that bias.
    evals = {}
    eta = fit_eta(df[df.split == "train"])
    summary["eta"] = eta
    for subset, e in (("all", None), ("ips", eta)):
        part = test
        per = {name: per_request(part, col, e) for name, col in rows.items()}
        # Each model is compared with default_linear at its own stage.
        evals[subset] = {
            "table": {name: (p.ndcg10.mean(), p.rr.mean()) for name, p in per.items()},
            "diffs": {
                m: bootstrap_diff(
                    per[m].ndcg10.to_numpy(), per[spec["baseline"]].ndcg10.to_numpy(), args.seed
                )
                for m, spec in MODELS.items()
            },
            "requests": part.request_id.nunique(),
        }
    table = evals["all"]["table"]

    trained_at = dt.datetime.now(dt.UTC).strftime("%Y-%m-%dT%H:%M:%SZ")
    for model_id, r in results.items():
        baseline = MODELS[model_id]["baseline"]
        manifest = {
            "id": model_id,
            "type": r["type"],
            "features": r["features"],
            "sha256": r["sha256"],
            "trained_at": trained_at,
            "dataset": f"{args.data.name} sha256:{sha256(args.data)[:16]}",
            "metrics": {
                "test_ndcg10": round(table[model_id][0], 4),
                "test_mrr": round(table[model_id][1], 4),
                "baseline_ndcg10": round(table[baseline][0], 4),
                "ips_test_ndcg10": round(evals["ips"]["table"][model_id][0], 4),
                "ips_baseline_ndcg10": round(evals["ips"]["table"][baseline][0], 4),
                "valid_ndcg10": round(r["valid_ndcg10"], 4),
                "learning_rate": r["learning_rate"],
                "trees": r["trees"],
            },
        }
        (r["dir"] / "features.json").write_text(json.dumps(manifest, indent=2) + "\n")

    report = write_report(args, summary, evals, results, time.time() - t0, trained_at)
    print(report.read_text())
    lo, _ = evals["all"]["diffs"]["rank-gbdt-v1"]
    if lo <= 0:
        raise SystemExit("rank-gbdt-v1 does not beat default_linear on the test split")


def booster_at(booster: lgb.Booster, trees: int) -> lgb.Booster:
    """The booster truncated to its best iteration (early stopping), for export."""
    return lgb.Booster(model_str=booster.model_to_string(num_iteration=trees))


def write_report(args, summary, evals, results, took, trained_at) -> Path:
    args.reports.mkdir(parents=True, exist_ok=True)
    path = args.reports / f"{trained_at[:10]}_ranking.md"
    lines = [
        f"# Ranking eval — {trained_at[:10]}",
        "",
        f"Dataset `{args.data.name}`: {summary['rows']:,} impressions; requests "
        f"train {summary['requests']['train']:,} / valid {summary['requests']['valid']:,} / "
        f"test {summary['requests']['test']:,} (split by time); "
        f"{summary['positives']:.1%} of impressions have a click, cart or order. "
        "Features come from the serving pipeline (`featurejob dataset`); online item features are "
        "aggregated as of the end of the training period. Metrics re-rank each test request's "
        "logged impressions (graded: order 3, cart 2, click 1).",
        "",
    ]
    titles = {
        "all": "Test split (held out)",
        "ips": "Test split, inverse propensity weighted (position-debiased)",
    }
    notes = {
        "all": "Logged clicks are position-biased (users mostly examine the top), which "
        "favours the logged order; compare the rankers with each other.",
        "ips": f"Each gain is divided by the examination propensity of its logged position, "
        f"pos^-{summary['eta']:.3f} (eta fitted on the training split's randomized requests, "
        f"weights capped at {MAX_IPS_WEIGHT:.0f}), which removes most of the logged order's "
        "advantage from position bias.",
    }
    for subset, ev in evals.items():
        lines += [
            f"## {titles[subset]} ({ev['requests']:,} requests)",
            "",
            notes[subset],
            "",
            "| Ranker | nDCG@10 | MRR | Δ nDCG@10 vs default_linear at its stage (95% CI) |",
            "|---|---|---|---|",
        ]
        for name, (nd, mrr) in ev["table"].items():
            ci = ""
            if name in ev["diffs"]:
                lo, hi = ev["diffs"][name]
                base = ev["table"][MODELS[name]["baseline"]][0]
                ci = f"{nd - base:+.4f} [{lo:+.4f}, {hi:+.4f}]"
            lines.append(f"| {name} | {nd:.4f} | {mrr:.4f} | {ci} |")
        lines.append("")
    lines += ["## Models", ""]
    for model_id, r in results.items():
        top = ", ".join(f"{n} ({g:.0f})" for n, g in r["importance"][:6])
        lines += [
            f"**{model_id}** ({r['type']}): {len(r['features'])} features, {r['trees']} trees at "
            f"learning rate {r['learning_rate']} (picked by validation nDCG@10 "
            f"{r['valid_ndcg10']:.4f}); ONNX parity {r['parity']:.1e}; "
            f"{r['infer_ms_2000']:.2f} ms for 2,000 candidates (ONNX Runtime, 1 thread); "
            f"top features by gain: {top}. sha256 `{r['sha256'][:16]}…`",
            "",
        ]
    lines += [f"Trained in {took:.0f} s with LightGBM {lgb.__version__}, seed {args.seed}.", ""]
    path.write_text("\n".join(lines))
    return path


if __name__ == "__main__":
    main()
