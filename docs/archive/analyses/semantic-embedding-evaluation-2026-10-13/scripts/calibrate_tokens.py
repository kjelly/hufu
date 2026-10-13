"""Calibrate a rune-based token estimate against real Ollama token counts.

estimate = cjk_runes * a + other_runes * b. Prints fitted coefficients and how
often a conservative estimate under-counts. Prints statistics only.
"""
import json
import random
import re
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import bench  # noqa: E402

CJK = re.compile(r"[㐀-鿿豈-﫿　-〿＀-￯]")
dataset = json.loads(Path(sys.argv[1]).read_text())
rng = random.Random(bench.SEED + 1)
goals = [q["goal"] for q in dataset["queries"] if len(q["goal"]) <= 3000]
zh = [g for g in goals if CJK.search(g)]
sample = rng.sample(zh, min(60, len(zh))) + rng.sample([g for g in goals if not CJK.search(g)], 60)
out = {}
for model in bench.PROFILES:
    prefix = bench.PROFILES[model]["query_prefix"]
    base, err, _ = bench.embed(model, prefix)
    prefix_tokens = base["prompt_eval_count"]
    rows = []
    for goal in sample:
        res, err, _ = bench.embed(model, prefix + goal)
        if err:
            continue
        cjk = len(CJK.findall(goal))
        rows.append((cjk, len(goal) - cjk, res["prompt_eval_count"] - prefix_tokens))
    # Least squares without intercept for tokens ~ a*cjk + b*other.
    sxx = sum(c * c for c, o, t in rows); syy = sum(o * o for c, o, t in rows); sxy = sum(c * o for c, o, t in rows)
    sxt = sum(c * t for c, o, t in rows); syt = sum(o * t for c, o, t in rows)
    det = sxx * syy - sxy * sxy
    a = (sxt * syy - syt * sxy) / det
    b = (syt * sxx - sxt * sxy) / det
    ratios = sorted(t / max(1e-9, a * c + b * o) for c, o, t in rows)
    p95, mx = ratios[int(len(ratios) * .95)], ratios[-1]
    cons = {"a": round(a * p95, 3), "b": round(b * p95, 3)}
    under = sum(t > cons["a"] * c + cons["b"] * o for c, o, t in rows)
    out[model] = {"n": len(rows), "prefix_tokens": prefix_tokens, "fit": {"a_cjk": round(a, 3), "b_other": round(b, 3)},
                  "actual_over_fit_p95": round(p95, 3), "actual_over_fit_max": round(mx, 3),
                  "conservative": cons, "conservative_undercount": under}
    bench.log(f"{model} {out[model]}")
Path(sys.argv[2]).write_text(json.dumps(out, indent=1))
