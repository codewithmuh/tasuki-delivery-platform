"""PromQL helpers for checking dashboards: Grafana variable substitution and metric-name extraction.

The extraction is a deliberately small lexer, not a parser: promtool checks that each expression is valid
PromQL, and this module only has to find the metric names in an expression that promtool already accepts.
"""

from __future__ import annotations

import re

# Values that make a dashboard query plain PromQL. The values only need to be valid, not realistic.
GRAFANA_VARIABLES = {
    "__rate_interval": "5m",
    "__interval": "1m",
    "__range": "1h",
    "namespace": "tasuki-prod",
    "service": "orders",
    "route": ".*",
}

_VARIABLE = re.compile(r"\$\{(\w+)\}|\$(\w+)")
_STRING = re.compile(r'"(?:\\.|[^"\\])*"|\'(?:\\.|[^\'\\])*\'|`[^`]*`')
_GROUPING = re.compile(r"\b(?:by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)")
_IDENTIFIER = re.compile(r"(?<![\w.:])([a-zA-Z_:][\w:]*)(\s*\()?")
_KEYWORDS = frozenset(
    {"and", "or", "unless", "by", "without", "on", "ignoring", "group_left", "group_right", "bool", "offset"}
    | {"inf", "nan", "atan2"}
)
_LABEL_VALUES = re.compile(r"^label_values\((.*),\s*\w+\)$", re.DOTALL)
HISTOGRAM_SUFFIXES = ("_bucket", "_count", "_sum")


def substitute(expr: str) -> str:
    """Replace Grafana variables with sample values. Raises KeyError for a variable this module does not know."""

    def value(match: re.Match[str]) -> str:
        name = match.group(1) or match.group(2)
        if name not in GRAFANA_VARIABLES:
            raise KeyError(f"unknown Grafana variable ${name} in: {expr}")
        return GRAFANA_VARIABLES[name]

    return _VARIABLE.sub(value, expr)


def variable_selector(query: str) -> str:
    """The PromQL inside a `label_values(<selector>, <label>)` variable query."""
    match = _LABEL_VALUES.match(query.strip())
    if not match:
        raise ValueError(f"expected label_values(<selector>, <label>), got: {query}")
    return match.group(1)


def metric_names(expr: str) -> set[str]:
    """Metric names (and recording-rule names) referenced by a valid PromQL expression."""
    text = _STRING.sub('""', expr)
    text = re.sub(r"\{[^}]*\}", " ", text)  # label matchers
    text = re.sub(r"\[[^\]]*\]", " ", text)  # range selectors and subqueries
    text = _GROUPING.sub(" ", text)  # by (...), on (...), group_left (...)
    names = set()
    for match in _IDENTIFIER.finditer(text):
        word, is_call = match.group(1), match.group(2)
        if is_call or word.lower() in _KEYWORDS:
            continue
        names.add(word)
    return names


def base_name(name: str, histograms: set[str]) -> str:
    """`foo_bucket` → `foo` when `foo` is a known histogram; any other name unchanged."""
    for suffix in HISTOGRAM_SUFFIXES:
        if name.endswith(suffix) and name[: -len(suffix)] in histograms:
            return name[: -len(suffix)]
    return name
