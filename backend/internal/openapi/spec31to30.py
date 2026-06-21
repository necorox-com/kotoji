#!/usr/bin/env python3
"""Convert a FROZEN OpenAPI 3.1 spec to a 3.0-compatible COPY for oapi-codegen.

oapi-codegen v2.4.1 (kin-openapi v0.127) cannot parse OpenAPI 3.1's nullable
type unions. This script reads docs/contracts/openapi.yaml (NEVER mutating it),
lowers it to a 3.0-equivalent form, and writes the result to a temp path:

  - `openapi: 3.1.0`            -> `openapi: 3.0.3`
  - `type: ["string", "null"]` -> `type: string` + `nullable: true`
  - `type: ["null", "string"]` -> same (order-independent)

These are pure-nullability rewrites; the wire shape and required/optional sets
are preserved, so the generated Go DTOs still match CANONICAL.md §8 exactly.

Usage: spec31to30.py <src.yaml> <dst.yaml>
"""
import sys

# Standard-library YAML round-trip keeps the dependency surface at zero.
import importlib

try:
    yaml = importlib.import_module("yaml")
except ImportError:  # pragma: no cover - PyYAML is part of the toolchain image
    sys.stderr.write("spec31to30: PyYAML is required\n")
    sys.exit(2)


def lower_nullable(node):
    """Recursively rewrite 3.1 nullable unions into 3.0 nullable flags."""
    if isinstance(node, dict):
        t = node.get("type")
        # A list-valued `type` containing "null" is the 3.1 nullable union form.
        if isinstance(t, list) and "null" in t:
            non_null = [x for x in t if x != "null"]
            # Exactly one concrete type collapses to `type: X, nullable: true`.
            # (Every union in this spec is a single type + null.)
            node["type"] = non_null[0] if len(non_null) == 1 else non_null
            node["nullable"] = True
        for v in node.values():
            lower_nullable(v)
    elif isinstance(node, list):
        for v in node:
            lower_nullable(v)
    return node


def disambiguate_parameters(spec):
    """Give path-parameter components a distinct Go type name.

    oapi-codegen derives a Go type per reusable parameter component. Several of
    them (Handle, Branch, ...) collide with schema component names of the same
    spelling, which aborts generation. We suffix the PARAMETER-derived type with
    `Param` via x-go-name so the schema keeps the clean name. This only affects
    the (unused) parameter type aliases; the schema DTOs we consume are intact.
    """
    params = spec.get("components", {}).get("parameters", {})
    for name, node in params.items():
        if isinstance(node, dict):
            node["x-go-name"] = f"{name}Param"


def main():
    if len(sys.argv) != 3:
        sys.stderr.write("usage: spec31to30.py <src.yaml> <dst.yaml>\n")
        sys.exit(2)
    src, dst = sys.argv[1], sys.argv[2]
    with open(src, "r", encoding="utf-8") as f:
        spec = yaml.safe_load(f)
    if str(spec.get("openapi", "")).startswith("3.1"):
        spec["openapi"] = "3.0.3"
    lower_nullable(spec)
    disambiguate_parameters(spec)
    with open(dst, "w", encoding="utf-8") as f:
        yaml.safe_dump(spec, f, sort_keys=False, allow_unicode=True)


if __name__ == "__main__":
    main()
