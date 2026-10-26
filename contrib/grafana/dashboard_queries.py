#!/usr/bin/env python3
"""Print the dashboard's PromQL queries as a Prometheus rule file.

`promtool check rules` then parses every query, which catches syntax errors
without a running Prometheus. Grafana variables are replaced with values that
parse.
"""
import json
import sys

SUBSTITUTIONS = {
    "$__rate_interval": "5m",
    "$job": ".*",
    "$instance": ".*",
}


def queries(panels):
    for panel in panels:
        for target in panel.get("targets", []):
            if target.get("datasource", {}).get("type") == "prometheus":
                yield panel["title"], target["expr"]
        yield from queries(panel.get("panels", []))


def main(path):
    with open(path) as f:
        dashboard = json.load(f)
    rules = []
    for i, (title, expr) in enumerate(queries(dashboard["panels"])):
        for var, value in SUBSTITUTIONS.items():
            expr = expr.replace(var, value)
        if "$" in expr:
            sys.exit(f"{title}: unknown variable in {expr!r}")
        rules.append({"record": f"dashboard:query_{i}", "expr": expr})
    if not rules:
        sys.exit("no Prometheus queries found")
    # JSON is valid YAML.
    json.dump({"groups": [{"name": "dashboard", "rules": rules}]}, sys.stdout, indent=1)


if __name__ == "__main__":
    main(sys.argv[1])
