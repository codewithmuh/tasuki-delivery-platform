"""Check the committed dashboards (generated/*.json): structure, data sources, links, metrics and PromQL.

    python3 observability/dashboards/src/lint.py [--promtool PATH]

Checks:
  - UIDs are unique, equal to the file name (so they never change by accident) and Grafana-valid.
  - Every dashboard has the `datasource` and `namespace` variables, `namespace` defaulting to tasuki-prod.
  - Every panel has a description, a unit, and uses the datasource variable; no data source UID is hard-coded.
  - Panels sit inside the 24-column grid without overlapping; panel IDs are unique.
  - Links to repository files point at files that exist (runbooks above all); links to dashboards at real UIDs.
  - Every metric a query uses is listed in src/metrics.py, and each listed in-repo metric exists in its source.
  - The SLO list in src/slos.py matches Sloth's generated rules (IDs, objectives, runbooks).
  - promtool parses every query (panels, annotations, variables) as PromQL, after substituting Grafana variables.
"""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
import tempfile
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import promql
from build import GENERATED, REPO, queries
from grafana import DS, GRID_COLUMNS, REPO_URL
from metrics import BY_NAME, HISTOGRAMS, METRICS
from slos import SLOS

UID_PATTERN = re.compile(r"^[a-z0-9-]{1,40}$")
MAX_BYTES = 512 * 1024  # a ConfigMap holds at most 1 MiB; stay well under it
VISUAL_TYPES = {"timeseries", "stat", "bargauge", "gauge", "heatmap"}
SOURCE_SUFFIXES = {".go", ".py", ".yaml", ".yml"}


class Report:
    def __init__(self) -> None:
        self.errors: list[str] = []
        self.warnings: list[str] = []

    def error(self, message: str) -> None:
        self.errors.append(message)

    def warn(self, message: str) -> None:
        self.warnings.append(message)


def walk(node: Any, path: str = "") -> Iterator[tuple[str, str, Any]]:
    """Yield (path, key, value) for every key of every object in a JSON tree."""
    if isinstance(node, dict):
        for key, value in node.items():
            yield path, key, value
            yield from walk(value, f"{path}.{key}")
    elif isinstance(node, list):
        for i, value in enumerate(node):
            yield from walk(value, f"{path}[{i}]")


def check_structure(name: str, model: dict, uids: set[str], r: Report) -> None:
    if model.get("uid") != name:
        r.error(f"{name}: uid {model.get('uid')!r} must equal the file name, so it stays stable")
    if not UID_PATTERN.match(name):
        r.error(f"{name}: uid must be 1-40 characters of a-z, 0-9 and '-'")

    variables = {v["name"]: v for v in model["templating"]["list"]}
    ds = variables.get("datasource")
    if not ds or ds["type"] != "datasource" or ds["query"] != "prometheus":
        r.error(f"{name}: needs a `datasource` variable of type datasource for prometheus")
    namespace = variables.get("namespace")
    if not namespace or namespace["current"].get("value") != "tasuki-prod":
        r.error(f"{name}: needs a `namespace` variable defaulting to tasuki-prod")

    for path, key, value in walk(model):
        if key == "datasource" and isinstance(value, dict) and "uid" in value and value != DS:
            r.error(f"{name}{path}: hard-coded data source {value}; use {DS}")

    ids: set[int] = set()
    boxes: list[tuple[str, dict]] = []
    for panel in model["panels"]:
        title = panel.get("title", "?")
        where = f"{name}: panel '{title}'"
        if panel["id"] in ids:
            r.error(f"{where}: duplicate panel id {panel['id']}")
        ids.add(panel["id"])
        grid = panel["gridPos"]
        if grid["x"] < 0 or grid["w"] < 1 or grid["x"] + grid["w"] > GRID_COLUMNS:
            r.error(f"{where}: gridPos {grid} is outside the {GRID_COLUMNS}-column grid")
        for other_title, other in boxes:
            if _overlap(grid, other):
                r.error(f"{where}: overlaps panel '{other_title}'")
        boxes.append((title, grid))
        if panel["type"] == "row":
            continue
        if len(panel.get("description", "")) < 30:
            r.error(f"{where}: needs a description of what it shows and why it matters")
        if panel.get("datasource") != DS:
            r.error(f"{where}: panel data source must be {DS}")
        if not panel.get("targets"):
            r.error(f"{where}: has no queries")
        for target in panel.get("targets", []):
            if target.get("datasource") != DS:
                r.error(f"{where}: query {target.get('refId')} data source must be {DS}")
            if not target.get("legendFormat"):
                r.error(f"{where}: query {target.get('refId')} needs a legend")
        if panel["type"] in VISUAL_TYPES and not panel["fieldConfig"]["defaults"].get("unit"):
            r.error(f"{where}: needs a unit")

    for _, key, value in walk(model):
        if key != "url" or not isinstance(value, str):
            continue
        if value.startswith(REPO_URL + "/"):
            target = value[len(REPO_URL) + 1 :].split("#")[0]
            if not (REPO / target).exists():
                r.error(f"{name}: link to a missing file: {target}")
        elif value.startswith("/d/"):
            linked = value[3:].split("?")[0]
            if linked not in uids:
                r.error(f"{name}: link to an unknown dashboard uid: {linked}")


def _overlap(a: dict, b: dict) -> bool:
    return (
        a["x"] < b["x"] + b["w"] and b["x"] < a["x"] + a["w"] and a["y"] < b["y"] + b["h"] and b["y"] < a["y"] + a["h"]
    )


def check_queries(name: str, model: dict, used: dict[str, set[str]], exprs: list[tuple[str, str]], r: Report) -> None:
    for where, expr in queries(model):
        try:
            plain = promql.substitute(expr)
        except KeyError as err:
            r.error(f"{name}: {where}: {err.args[0]}")
            continue
        exprs.append((f"{name}: {where}", plain))
        for metric in promql.metric_names(plain):
            base = promql.base_name(metric, HISTOGRAMS)
            if base not in BY_NAME:
                r.error(f"{name}: {where}: metric {metric!r} is not listed in src/metrics.py")
            used.setdefault(base, set()).add(name)


def _contains(path: Path, needle: str) -> bool:
    if path.is_file():
        return needle in path.read_text(encoding="utf-8", errors="replace")
    if path.is_dir():
        return any(
            needle in f.read_text(encoding="utf-8", errors="replace")
            for f in path.rglob("*")
            if f.is_file() and f.suffix in SOURCE_SUFFIXES and not f.name.endswith("_test.go")
        )
    return False


def check_metrics(used: dict[str, set[str]], r: Report) -> None:
    for metric in METRICS:
        if metric.name not in used:
            r.error(f"metrics.py: {metric.name} is listed but no dashboard uses it")
        if not metric.sources:
            continue
        found = any(_contains(REPO / source, metric.name) for source in metric.sources)
        sources = ", ".join(metric.sources)
        if found and metric.pending:
            r.warn(f"{metric.name} now exists in {sources}: drop pending=True in src/metrics.py")
        elif not found and metric.pending:
            r.warn(f"{metric.name} is not in {sources} yet (pending: agreed with the service's owner)")
        elif not found:
            r.error(f"{metric.name} is not defined in {sources}")


def check_slos(r: Report) -> None:
    text = "\n".join(
        p.read_text(encoding="utf-8") for p in sorted((REPO / "observability/slo/generated").glob("*.yaml"))
    )
    objectives = {}
    for chunk in text.split("- record: sloth_slo_info")[1:]:
        labels = chunk.split("- ")[0]
        sloth_id = re.search(r"sloth_id: (\S+)", labels)
        objective = re.search(r'sloth_objective: "?([\d.]+)"?', labels)
        if sloth_id and objective:
            objectives[sloth_id.group(1)] = float(objective.group(1))
    runbooks = {}
    for chunk in text.split("- alert: ")[1:]:
        sloth_id = re.search(r'sloth_id="([^"]+)"', chunk)
        runbook = re.search(r"runbook_url: \S+/docs/runbooks/([\w-]+)\.md", chunk)
        if sloth_id and runbook:
            runbooks[sloth_id.group(1)] = runbook.group(1)

    ours = {slo.sloth_id: slo for slo in SLOS}
    for missing in sorted(set(objectives) - set(ours)):
        r.error(f"slos.py: Sloth defines SLO {missing}; add it to SLOS")
    for extra in sorted(set(ours) - set(objectives)):
        r.error(f"slos.py: SLO {extra} is not in observability/slo/generated")
    for sloth_id, slo in ours.items():
        if sloth_id in objectives and objectives[sloth_id] != slo.objective:
            r.error(f"slos.py: {sloth_id} objective {slo.objective} != Sloth's {objectives[sloth_id]}")
        if sloth_id in runbooks and runbooks[sloth_id] != slo.runbook:
            r.error(f"slos.py: {sloth_id} runbook {slo.runbook} != the alert's {runbooks[sloth_id]}")


def check_promql(exprs: list[tuple[str, str]], promtool: str, r: Report) -> None:
    """promtool parses PromQL only inside rule files, so each query becomes a recording rule."""
    lines = ["groups:", "  - name: dashboard-queries", "    rules:"]
    for i, (_, expr) in enumerate(exprs):
        lines += [f"      - record: dashboard_query_{i}", "        expr: |"]
        lines += [f"          {line}" for line in expr.splitlines()]
    with tempfile.TemporaryDirectory() as tmp:
        rules = Path(tmp) / "dashboard-queries.yaml"
        rules.write_text("\n".join(lines) + "\n", encoding="utf-8")
        try:
            result = subprocess.run(
                [promtool, "check", "rules", str(rules)], capture_output=True, text=True, check=False
            )
        except FileNotFoundError:
            r.error(f"promtool not found at {promtool!r}; set PROMTOOL (make) or --promtool")
            return
    if result.returncode != 0:
        output = result.stdout + result.stderr
        r.error(f"promtool rejected the dashboard queries:\n{output}")
        for i, (where, expr) in enumerate(exprs):
            if re.search(rf'"dashboard_query_{i}"', output):
                r.error(f"  dashboard_query_{i} = {where}: {expr}")
    else:
        print(f"promtool: {len(exprs)} dashboard queries parse as PromQL")


def main() -> int:
    parser = argparse.ArgumentParser(description="Lint the generated Grafana dashboards.")
    parser.add_argument("--promtool", default="promtool", help="path to promtool (Prometheus 3.x)")
    args = parser.parse_args()

    r = Report()
    files = sorted(GENERATED.glob("*.json"))
    if not files:
        r.error(f"no dashboards in {GENERATED}")
    models = {}
    for path in files:
        if path.stat().st_size > MAX_BYTES:
            r.error(
                f"{path.name}: {path.stat().st_size} bytes; keep dashboards under {MAX_BYTES} (ConfigMap limit 1 MiB)"
            )
        models[path.stem] = json.loads(path.read_text(encoding="utf-8"))
    titles = [m["title"] for m in models.values()]
    if len(titles) != len(set(titles)):
        r.error(f"dashboard titles must be unique: {titles}")

    used: dict[str, set[str]] = {}
    exprs: list[tuple[str, str]] = []
    for name, model in models.items():
        check_structure(name, model, set(models), r)
        check_queries(name, model, used, exprs, r)
    check_metrics(used, r)
    check_slos(r)
    check_promql(exprs, args.promtool, r)

    for warning in r.warnings:
        print(f"warning: {warning}")
    for error in r.errors:
        print(f"error: {error}", file=sys.stderr)
    if r.errors:
        print(f"dashboards: {len(r.errors)} error(s)", file=sys.stderr)
        return 1
    panels = sum(len([p for p in m["panels"] if p["type"] != "row"]) for m in models.values())
    print(f"dashboards: {len(models)} dashboards, {panels} panels, {len(used)} metrics: OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
