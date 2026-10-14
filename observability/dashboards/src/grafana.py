"""Small builders for Grafana dashboard JSON (schema 39, Grafana 11+). Python standard library only.

A dashboard module describes panels with these helpers; `Dashboard.add` places them on Grafana's 24-column grid
left to right, wrapping to a new line when a panel does not fit, so no module hand-writes coordinates or IDs.

Every panel and every query uses the `datasource` template variable: no dashboard names a data source UID.
"""

from __future__ import annotations

import textwrap
from typing import Any

SCHEMA_VERSION = 39
GRID_COLUMNS = 24
REPO_URL = "https://github.com/codewithmuh/tasuki-delivery-platform/blob/main"

# The only data source reference any panel or query may carry (lint.py enforces it).
DS: dict[str, str] = {"type": "prometheus", "uid": "${datasource}"}


Json = dict[str, Any]


# --- Links ---------------------------------------------------------------------------------------------------


def runbook(name: str) -> Json:
    """Panel link to docs/runbooks/<name>.md (lint.py checks that the file exists)."""
    return {"title": f"Runbook: {name}", "url": f"{REPO_URL}/docs/runbooks/{name}.md", "targetBlank": True}


def doc(title: str, path: str) -> Json:
    """Panel link to any file in the repository."""
    return {"title": title, "url": f"{REPO_URL}/{path}", "targetBlank": True}


def dashboard(title: str, uid: str, extra_vars: str = "") -> Json:
    """Panel link to another Tasuki dashboard, keeping the time range, data source and namespace."""
    url = f"/d/{uid}?${{__url_time_range}}&var-datasource=${{datasource}}&var-namespace=${{namespace}}{extra_vars}"
    return {"title": title, "url": url}


# --- Queries and thresholds ----------------------------------------------------------------------------------


def q(expr: str, legend: str, *, instant: bool = False, fmt: str = "time_series") -> Json:
    """One Prometheus query. `legend` is required: series names must mean something on a screenshot."""
    target: Json = {
        "datasource": DS,
        "editorMode": "code",
        "expr": textwrap.dedent(expr).strip(),
        "legendFormat": legend,
        "range": not instant,
        "instant": instant,
    }
    if fmt != "time_series":
        target["format"] = fmt
    return target


def steps(base: str, *levels: tuple[float, str]) -> list[Json]:
    """Threshold steps: `base` colour below the first level, then (value, colour) pairs in ascending order."""
    return [{"color": base, "value": None}] + [{"color": color, "value": value} for value, color in levels]


def lines(*levels: tuple[float, str]) -> list[Json]:
    """Thresholds drawn as dashed lines on a time series (an SLO bound, an alert threshold)."""
    return steps("transparent", *levels)


# --- Panels --------------------------------------------------------------------------------------------------


def _with_ref_ids(targets: list[Json]) -> list[Json]:
    return [{**t, "refId": chr(ord("A") + i)} for i, t in enumerate(targets)]


def _panel(kind: str, title: str, description: str, targets: list[Json], links: list[Json] | None) -> Json:
    panel: Json = {
        "type": kind,
        "title": title,
        # One paragraph: Grafana renders descriptions as Markdown in the panel's info tooltip.
        "description": " ".join(description.split()),
        "datasource": DS,
        "targets": _with_ref_ids(targets),
    }
    if links:
        panel["links"] = links
    return panel


def _defaults(
    unit: str,
    *,
    color: str,
    thresholds: list[Json] | None,
    min_: float | None,
    max_: float | None,
    decimals: int | None,
) -> Json:
    defaults: Json = {
        "unit": unit,
        "color": {"mode": color},
        "thresholds": {"mode": "absolute", "steps": thresholds or steps("green")},
    }
    if min_ is not None:
        defaults["min"] = min_
    if max_ is not None:
        defaults["max"] = max_
    if decimals is not None:
        defaults["decimals"] = decimals
    return defaults


def timeseries(
    title: str,
    description: str,
    targets: list[Json],
    *,
    unit: str,
    thresholds: list[Json] | None = None,
    stack: bool = False,
    bars: bool = False,
    min_: float | None = 0,
    max_: float | None = None,
    decimals: int | None = None,
    dashed: list[str] | None = None,
    links: list[Json] | None = None,
) -> Json:
    """A time series. Pass `thresholds=lines(...)` to draw SLO bounds or alert thresholds as dashed lines,
    and `dashed=[legend, ...]` to draw reference series (a request, a limit) as red dashed lines."""
    panel = _panel("timeseries", title, description, targets, links)
    defaults = _defaults(unit, color="palette-classic", thresholds=thresholds, min_=min_, max_=max_, decimals=decimals)
    defaults["custom"] = {
        "drawStyle": "bars" if bars else "line",
        "lineWidth": 1 if stack else 2,
        "fillOpacity": 30 if (stack or bars) else 5,
        "showPoints": "never",
        "spanNulls": False,
        "stacking": {"mode": "normal" if stack else "none", "group": "A"},
        "thresholdsStyle": {"mode": "dashed" if thresholds else "off"},
    }
    overrides = [
        {
            "matcher": {"id": "byName", "options": name},
            "properties": [
                {"id": "custom.lineStyle", "value": {"fill": "dash", "dash": [10, 10]}},
                {"id": "custom.fillOpacity", "value": 0},
                {"id": "color", "value": {"mode": "fixed", "fixedColor": "red"}},
            ],
        }
        for name in dashed or []
    ]
    panel["fieldConfig"] = {"defaults": defaults, "overrides": overrides}
    panel["options"] = {
        "legend": {"displayMode": "list", "placement": "bottom", "showLegend": True},
        "tooltip": {"mode": "multi", "sort": "desc"},
    }
    return panel


def stat(
    title: str,
    description: str,
    targets: list[Json],
    *,
    unit: str,
    thresholds: list[Json] | None = None,
    decimals: int | None = None,
    graph: bool = True,
    links: list[Json] | None = None,
) -> Json:
    """A single number, coloured by its thresholds, with a sparkline unless graph=False."""
    panel = _panel("stat", title, description, targets, links)
    defaults = _defaults(unit, color="thresholds", thresholds=thresholds, min_=None, max_=None, decimals=decimals)
    panel["fieldConfig"] = {"defaults": defaults, "overrides": []}
    panel["options"] = {
        "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
        "colorMode": "value",
        "graphMode": "area" if graph else "none",
        "justifyMode": "auto",
        "orientation": "auto",
        "textMode": "auto",
    }
    return panel


def bargauge(
    title: str,
    description: str,
    targets: list[Json],
    *,
    unit: str,
    thresholds: list[Json],
    min_: float | None = None,
    max_: float | None = None,
    decimals: int | None = None,
    links: list[Json] | None = None,
) -> Json:
    """Horizontal bars, one per series: good for "every SLO side by side"."""
    panel = _panel("bargauge", title, description, targets, links)
    defaults = _defaults(unit, color="thresholds", thresholds=thresholds, min_=min_, max_=max_, decimals=decimals)
    panel["fieldConfig"] = {"defaults": defaults, "overrides": []}
    panel["options"] = {
        "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
        "displayMode": "gradient",
        "orientation": "horizontal",
        "showUnfilled": True,
        "valueMode": "color",
    }
    return panel


def heatmap(title: str, description: str, expr: str, *, unit: str, links: list[Json] | None = None) -> Json:
    """A heatmap of a classic histogram: `expr` must return per-bucket increases `sum by (le) (increase(...))`."""
    panel = _panel("heatmap", title, description, [q(expr, "{{le}}", fmt="heatmap")], links)
    panel["fieldConfig"] = {"defaults": {"unit": unit}, "overrides": []}
    panel["options"] = {
        "calculate": False,
        "cellGap": 1,
        "color": {"mode": "scheme", "scheme": "Oranges", "scale": "exponential", "exponent": 0.5, "steps": 64},
        "yAxis": {"axisPlacement": "left", "unit": unit},
        "rowsFrame": {"layout": "auto"},
        "tooltip": {"mode": "single", "yHistogram": True},
        "legend": {"show": True},
        "filterValues": {"le": 1e-9},
    }
    return panel


def table(
    title: str,
    description: str,
    targets: list[Json],
    *,
    hide: list[str],
    rename: dict[str, str] | None = None,
    field_links: dict[str, list[Json]] | None = None,
    links: list[Json] | None = None,
) -> Json:
    """A table of an instant query's labels. `hide` drops columns; `field_links` adds data links to a column."""
    panel = _panel("table", title, description, targets, links)
    overrides = [
        {"matcher": {"id": "byName", "options": field}, "properties": [{"id": "links", "value": value}]}
        for field, value in (field_links or {}).items()
    ]
    panel["fieldConfig"] = {"defaults": {"unit": "none"}, "overrides": overrides}
    panel["options"] = {"showHeader": True, "cellHeight": "sm", "footer": {"show": False}}
    panel["transformations"] = [
        {
            "id": "organize",
            "options": {"excludeByName": dict.fromkeys(hide, True), "renameByName": rename or {}},
        }
    ]
    return panel


# --- Variables and annotations -------------------------------------------------------------------------------


def datasource_var() -> Json:
    """The Prometheus data source picker. `current` is empty, so Grafana selects the default data source."""
    return {
        "name": "datasource",
        "label": "Data source",
        "type": "datasource",
        "query": "prometheus",
        "current": {},
        "hide": 0,
        "includeAll": False,
        "multi": False,
        "options": [],
        "refresh": 1,
        "regex": "",
    }


def query_var(
    name: str,
    label: str,
    query: str,
    *,
    current: str,
    description: str,
    include_all: bool = False,
    all_value: str | None = None,
    multi: bool = False,
) -> Json:
    """A variable filled from `label_values(...)`. `current` is the default selection ("All" for include_all)."""
    if include_all and current == "All":
        value: Any = ["$__all"] if multi else "$__all"
        text: Any = ["All"] if multi else "All"
    else:
        value = [current] if multi else current
        text = value
    var: Json = {
        "name": name,
        "label": label,
        "description": description,
        "type": "query",
        "datasource": DS,
        "query": query,
        "definition": query,
        "current": {"selected": True, "text": text, "value": value},
        "hide": 0,
        "includeAll": include_all,
        "multi": multi,
        "options": [],
        "refresh": 2,  # on time range change: a namespace or route that appears later shows up
        "regex": "",
        "sort": 1,
    }
    if all_value is not None:
        var["allValue"] = all_value
    return var


def namespace_var() -> Json:
    return query_var(
        "namespace",
        "Namespace",
        'label_values(up{namespace=~"tasuki-.*"}, namespace)',
        current="tasuki-prod",
        description="tasuki-prod or tasuki-dev. Both share one cluster and one Prometheus; only prod has SLOs.",
    )


def alert_annotation(name: str, expr: str, color: str) -> Json:
    """Marks the time ranges in which the alerts matched by `expr` (an ALERTS selector) were firing."""
    return {
        "name": name,
        "datasource": DS,
        "enable": True,
        "hide": False,
        "iconColor": color,
        "expr": expr,
        "step": "60s",
        "titleFormat": "{{alertname}}",
        "textFormat": "{{severity}} {{sloth_service}} {{sloth_slo}} {{component}}",
        "tagKeys": "severity,category",
        "useValueForTime": False,
    }


# --- Dashboard -----------------------------------------------------------------------------------------------


class Dashboard:
    """Collects panels and lays them out on the grid in the order they are added."""

    def __init__(
        self,
        *,
        uid: str,
        title: str,
        description: str,
        tags: list[str],
        variables: list[Json],
        annotations: list[Json] | None = None,
        time_from: str = "now-3h",
    ) -> None:
        self.uid = uid
        self.title = title
        self.description = description
        self.tags = ["tasuki", *tags]
        self.variables = variables
        self.annotations = annotations or []
        self.time_from = time_from
        self.panels: list[Json] = []
        self._x = 0
        self._y = 0
        self._line_height = 0

    def _newline(self) -> None:
        if self._x:
            self._y += self._line_height
            self._x = 0
            self._line_height = 0

    def row(self, title: str) -> None:
        """Start a titled section on a new line."""
        self._newline()
        self.panels.append(
            {
                "id": len(self.panels) + 1,
                "type": "row",
                "title": title,
                "collapsed": False,
                "panels": [],
                "gridPos": {"h": 1, "w": GRID_COLUMNS, "x": 0, "y": self._y},
            }
        )
        self._y += 1

    def add(self, panel: Json, *, w: int, h: int) -> None:
        """Place `panel` right of the previous one, or on a new line when it does not fit."""
        if not 0 < w <= GRID_COLUMNS:
            raise ValueError(f"{panel['title']}: width {w} is outside 1..{GRID_COLUMNS}")
        if self._x + w > GRID_COLUMNS:
            self._newline()
        placed = {"id": len(self.panels) + 1, "gridPos": {"h": h, "w": w, "x": self._x, "y": self._y}, **panel}
        self.panels.append(placed)
        self._x += w
        self._line_height = max(self._line_height, h)

    def to_json(self) -> Json:
        return {
            "id": None,
            "uid": self.uid,
            "title": self.title,
            "description": self.description,
            "tags": self.tags,
            "editable": True,
            "graphTooltip": 1,  # shared crosshair: line up a spike across panels
            "refresh": "30s",
            "schemaVersion": SCHEMA_VERSION,
            "time": {"from": self.time_from, "to": "now"},
            "timezone": "browser",
            "version": 1,
            "links": [
                {
                    "title": "Tasuki dashboards",
                    "type": "dashboards",
                    "tags": ["tasuki"],
                    "asDropdown": True,
                    "includeVars": True,
                    "keepTime": True,
                },
                {
                    "title": "Runbooks",
                    "type": "link",
                    "url": f"{REPO_URL}/docs/runbooks/README.md",
                    "tooltip": "One runbook per alert",
                    "icon": "doc",
                    "targetBlank": True,
                },
            ],
            "templating": {"list": self.variables},
            "annotations": {"list": self.annotations},
            "panels": self.panels,
        }
