"""Unit tests for the helpers lint.py relies on. Run: python3 -m unittest discover -s observability/dashboards/src"""

from __future__ import annotations

import unittest

import promql
from grafana import Dashboard, q, timeseries


class MetricNames(unittest.TestCase):
    def names(self, expr: str) -> set[str]:
        return promql.metric_names(promql.substitute(expr))

    def test_selector_with_labels_and_range(self) -> None:
        expr = 'sum(rate(http_requests_total{namespace="$namespace", status=~"5.."}[$__rate_interval]))'
        self.assertEqual(self.names(expr), {"http_requests_total"})

    def test_grouping_labels_are_not_metrics(self) -> None:
        expr = "histogram_quantile(0.95, sum by (le, route) (rate(http_request_duration_seconds_bucket[5m])))"
        self.assertEqual(self.names(expr), {"http_request_duration_seconds_bucket"})

    def test_recording_rules_and_vector_matching(self) -> None:
        expr = 'slo:sli_error:ratio_rate1h{sloth_id="x"} / ignoring (sloth_window) slo:error_budget:ratio{sloth_id="x"}'
        self.assertEqual(self.names(expr), {"slo:sli_error:ratio_rate1h", "slo:error_budget:ratio"})

    def test_keywords_numbers_and_functions_are_skipped(self) -> None:
        expr = 'count(ALERTS{alertstate="firing"}) or vector(0) > bool 1e-3 unless on () up offset 5m'
        self.assertEqual(self.names(expr), {"ALERTS", "up"})

    def test_label_values_that_look_like_metrics(self) -> None:
        expr = 'kafka_consumergroup_lag{topic=~"orders.events|dispatch.events", consumergroup="fleet-state"}'
        self.assertEqual(self.names(expr), {"kafka_consumergroup_lag"})

    def test_histogram_suffix_maps_to_base_name(self) -> None:
        histograms = {"fleet_state_freshness_seconds"}
        self.assertEqual(
            promql.base_name("fleet_state_freshness_seconds_bucket", histograms), "fleet_state_freshness_seconds"
        )
        self.assertEqual(promql.base_name("orders_created_total", histograms), "orders_created_total")
        self.assertEqual(promql.base_name("http_requests_count", histograms), "http_requests_count")


class Substitution(unittest.TestCase):
    def test_known_variables(self) -> None:
        got = promql.substitute('rate(x{namespace="$namespace", service="${service}"}[$__rate_interval])')
        self.assertEqual(got, 'rate(x{namespace="tasuki-prod", service="orders"}[5m])')

    def test_unknown_variable_fails(self) -> None:
        with self.assertRaises(KeyError):
            promql.substitute('x{ward="$ward"}')

    def test_variable_selector(self) -> None:
        self.assertEqual(
            promql.variable_selector('label_values(up{namespace=~"tasuki-.*"}, namespace)'),
            'up{namespace=~"tasuki-.*"}',
        )
        with self.assertRaises(ValueError):
            promql.variable_selector("query_result(up)")


class Layout(unittest.TestCase):
    def panel(self, title: str) -> dict:
        return timeseries(title, "a description long enough to pass", [q("up", "up")], unit="short")

    def test_panels_flow_and_wrap(self) -> None:
        d = Dashboard(uid="t", title="t", description="t", tags=[], variables=[])
        d.row("first")
        d.add(self.panel("a"), w=12, h=8)
        d.add(self.panel("b"), w=12, h=6)
        d.add(self.panel("c"), w=8, h=4)  # does not fit: new line below the taller of a and b
        d.row("second")
        d.add(self.panel("d"), w=24, h=5)
        grid = {p["title"]: p["gridPos"] for p in d.to_json()["panels"]}
        self.assertEqual(grid["a"], {"h": 8, "w": 12, "x": 0, "y": 1})
        self.assertEqual(grid["b"], {"h": 6, "w": 12, "x": 12, "y": 1})
        self.assertEqual(grid["c"], {"h": 4, "w": 8, "x": 0, "y": 9})
        self.assertEqual(grid["second"], {"h": 1, "w": 24, "x": 0, "y": 13})
        self.assertEqual(grid["d"], {"h": 5, "w": 24, "x": 0, "y": 14})

    def test_ref_ids_are_assigned_in_order(self) -> None:
        panel = timeseries("t", "d", [q("up", "a"), q("up", "b")], unit="short")
        self.assertEqual([t["refId"] for t in panel["targets"]], ["A", "B"])


if __name__ == "__main__":
    unittest.main()
