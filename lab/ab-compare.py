"""ab-compare.py DIR: the verdict of lab/ab.sh's runs in DIR.

DIR holds a-R.metrics and b-R.metrics, one "name value" per line, from
round R of version A and B. Lower is better for every metric. For each
metric both versions have, it prints the medians, B's change from A, a 95%
interval for that change (bootstrap of the ratio of medians, seeded) and the
exact two-sided Mann-Whitney p. A change is "worse" or "better" only with
p < 0.05 and at least MIN_CHANGE; a metric that is 0 in A and not in B is
"from 0" (a count A doesn't keep); anything else is "same".
"""

import glob
import itertools
import os
import random
import statistics
import sys

MIN_CHANGE = 0.02
BOOTSTRAP = 2000


def load(dir, version):
    runs = {}
    for path in sorted(glob.glob(os.path.join(dir, f"{version}-*.metrics"))):
        with open(path) as f:
            for line in f:
                name, value = line.split()
                runs.setdefault(name, []).append(float(value))
    return runs


def mann_whitney_p(a, b):
    """The exact two-sided p of the rank-sum test, by enumerating every split."""
    pooled = sorted(a + b)
    def rank(v):
        lo = pooled.index(v)
        hi = len(pooled) - 1 - pooled[::-1].index(v)
        return (lo + hi) / 2 + 1
    ranks = [rank(v) for v in pooled]
    observed = sum(rank(v) for v in a)
    expected = len(a) * (len(pooled) + 1) / 2
    splits = list(itertools.combinations(ranks, len(a)))
    extreme = sum(1 for s in splits if abs(sum(s) - expected) >= abs(observed - expected) - 1e-9)
    return extreme / len(splits)


def interval(a, b, rng):
    ratios = []
    for _ in range(BOOTSTRAP):
        ma = statistics.median(rng.choices(a, k=len(a)))
        mb = statistics.median(rng.choices(b, k=len(b)))
        if ma > 0:
            ratios.append(mb / ma - 1)
    ratios.sort()
    if not ratios:
        return None
    return ratios[int(0.025 * len(ratios))], ratios[int(0.975 * len(ratios)) - 1]


def main(dir):
    a, b = load(dir, "a"), load(dir, "b")
    rng = random.Random(1)
    rows = []
    for name in sorted(set(a) & set(b)):
        va, vb = a[name], b[name]
        ma, mb = statistics.median(va), statistics.median(vb)
        change = mb / ma - 1 if ma else 0.0
        p = mann_whitney_p(va, vb) if len(va) > 1 and len(vb) > 1 and len(va) + len(vb) <= 20 else 1.0
        ci = interval(va, vb, rng)
        verdict = "same"
        if ma == 0 and mb != 0:
            # No change in percent from zero: a count that A doesn't keep.
            verdict = "from 0"
        elif p < 0.05 and abs(change) >= MIN_CHANGE:
            verdict = "worse" if change > 0 else "better"
        rows.append((verdict, p, name, ma, mb, change, ci, len(va), len(vb)))
    order = {"worse": 0, "better": 1, "from 0": 2, "same": 3}
    rows.sort(key=lambda r: (order[r[0]], r[1], r[2]))
    with open(os.path.join(dir, "versions")) as f:
        print("# A/B\n\n" + f.read().replace("\n", "  \n"))
    counts = {v: sum(1 for r in rows if r[0] == v) for v in order}
    print(f"**{counts['worse']} worse, {counts['better']} better, {counts['same']} same, {counts['from 0']} from 0** "
          f"(p < 0.05 and a change of at least {MIN_CHANGE:.0%})\n")
    print("| verdict | metric | A | B | change | 95% interval | p | n |")
    print("|---|---|---|---|---|---|---|---|")
    for verdict, p, name, ma, mb, change, ci, na, nb in rows:
        span = f"{ci[0]:+.1%} … {ci[1]:+.1%}" if ci else "–"
        print(f"| {verdict} | {name} | {ma:g} | {mb:g} | {change:+.1%} | {span} | {p:.3f} | {na}/{nb} |")
    only = sorted(set(a) ^ set(b))
    if only:
        print("\nOnly in one version: " + ", ".join(f"{n} ({'A' if n in a else 'B'})" for n in only))


if __name__ == "__main__":
    main(sys.argv[1])
