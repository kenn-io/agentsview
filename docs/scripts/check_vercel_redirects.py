#!/usr/bin/env python3
from __future__ import annotations

import json
import math
import pathlib
import sys
from typing import Any

ROOT = pathlib.Path(__file__).resolve().parents[1]
VERCEL = ROOT / "vercel.json"

PERMANENT: dict[str, str] = {}

TEMPORARY = {
    "/install.sh": "https://raw.githubusercontent.com/kenn-io/agentsview/main/scripts/install.sh",
    "/install.ps1": "https://raw.githubusercontent.com/kenn-io/agentsview/main/scripts/install.ps1",
}

MAX_REDIRECTS = 2048
MAX_CONDITIONS = 16
MAX_SCHEMA_STRING_LENGTH = 4096
MAX_ENV_ITEMS = 64
MAX_ENV_VALUE_LENGTH = 256

ALLOWED_REDIRECT_KEYS = {
    "source",
    "destination",
    "permanent",
    "statusCode",
    "has",
    "missing",
    "env",
}

ALLOWED_CONDITION_KEYS = {"type", "key", "value"}
CONDITION_TYPES_REQUIRING_KEY = {"header", "cookie", "query"}
CONDITION_TYPES = {"host", *CONDITION_TYPES_REQUIRING_KEY}
CONDITION_OPERATION_KEYS = {
    "eq",
    "neq",
    "inc",
    "ninc",
    "pre",
    "suf",
    "re",
    "gt",
    "gte",
    "lt",
    "lte",
}
STRING_CONDITION_OPERATIONS = {"neq", "pre", "suf", "re"}
STRING_LIST_CONDITION_OPERATIONS = {"inc", "ninc"}
NUMBER_CONDITION_OPERATIONS = {"gt", "gte", "lt", "lte"}


def fail(message: str) -> None:
    print(f"FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


def reject_json_constant(constant: str) -> None:
    raise ValueError(f"non-finite numeric constant {constant}")


def validate_schema_string(path: str, value: str) -> None:
    if len(value) > MAX_SCHEMA_STRING_LENGTH:
        fail(f"{path} must be at most {MAX_SCHEMA_STRING_LENGTH} characters")


def validate_finite_json_numbers(path: str, value: object) -> None:
    if isinstance(value, bool):
        return
    if isinstance(value, float):
        if not math.isfinite(value):
            fail(f"{path} contains non-finite number")
        return
    if isinstance(value, dict):
        for key, item in value.items():
            validate_finite_json_numbers(f"{path}.{key}", item)
    elif isinstance(value, list):
        for index, item in enumerate(value):
            validate_finite_json_numbers(f"{path}[{index}]", item)


def is_number(value: object) -> bool:
    if isinstance(value, bool):
        return False
    if isinstance(value, int):
        return True
    return isinstance(value, float) and math.isfinite(value)


def validate_condition_operation_value(path: str, operation: str, value: object) -> None:
    if operation == "eq":
        if not isinstance(value, str) and not is_number(value):
            fail(f"{path} eq must be a string or finite number")
        if isinstance(value, str):
            validate_schema_string(f"{path} eq", value)
    elif operation in STRING_CONDITION_OPERATIONS:
        if not isinstance(value, str):
            fail(f"{path} {operation} must be a string")
        validate_schema_string(f"{path} {operation}", value)
    elif operation in STRING_LIST_CONDITION_OPERATIONS:
        if not isinstance(value, list) or not all(isinstance(item, str) for item in value):
            fail(f"{path} {operation} must be a list of strings")
        for item_index, item in enumerate(value):
            validate_schema_string(f"{path} {operation}[{item_index}]", item)
    elif operation in NUMBER_CONDITION_OPERATIONS:
        if not is_number(value):
            fail(f"{path} {operation} must be a finite number")


def validate_condition_value(path: str, value: object) -> None:
    if isinstance(value, str):
        validate_schema_string(path, value)
        return
    if not isinstance(value, dict):
        fail(f"{path} must be a string or condition operation object")
    if not value:
        fail(f"{path} condition operation object must not be empty")
    unknown_keys = set(value) - CONDITION_OPERATION_KEYS
    if unknown_keys:
        keys = ", ".join(sorted(unknown_keys))
        fail(f"{path} has unknown operation keys: {keys}")
    for operation, operation_value in value.items():
        validate_condition_operation_value(path, operation, operation_value)


def validate_condition_list(index: int, item: dict[str, object], key: str) -> None:
    if key not in item:
        return

    conditions = item[key]
    if not isinstance(conditions, list):
        fail(f"redirect entry {index} {key} must be a list")
    if len(conditions) > MAX_CONDITIONS:
        fail(f"redirect entry {index} {key} must contain at most {MAX_CONDITIONS} items")

    for condition_index, condition in enumerate(conditions):
        if not isinstance(condition, dict):
            fail(f"redirect entry {index} {key}[{condition_index}] must be an object")
        unknown_keys = set(condition) - ALLOWED_CONDITION_KEYS
        if unknown_keys:
            keys = ", ".join(sorted(unknown_keys))
            fail(f"redirect entry {index} {key}[{condition_index}] has unknown keys: {keys}")
        condition_type = condition.get("type")
        if not isinstance(condition_type, str):
            fail(f"redirect entry {index} {key}[{condition_index}] type must be a string")
        if condition_type not in CONDITION_TYPES:
            fail(f"redirect entry {index} {key}[{condition_index}] has invalid type: {condition_type}")
        condition_key = condition.get("key")
        if condition_type in CONDITION_TYPES_REQUIRING_KEY:
            if not isinstance(condition_key, str) or not condition_key:
                fail(f"redirect entry {index} {key}[{condition_index}] missing key")
            validate_schema_string(
                f"redirect entry {index} {key}[{condition_index}] key",
                condition_key,
            )
        elif "key" in condition:
            fail(f"redirect entry {index} {key}[{condition_index}] host condition must not set key")
        elif "value" not in condition:
            fail(f"redirect entry {index} {key}[{condition_index}] host condition missing value")
        if "value" in condition:
            validate_condition_value(
                f"redirect entry {index} {key}[{condition_index}] value",
                condition["value"],
            )


def validate_redirect_mode(index: int, item: dict[str, object]) -> None:
    has_permanent = "permanent" in item
    has_status_code = "statusCode" in item
    if not has_permanent and not has_status_code:
        fail(f"redirect entry {index} must set permanent or statusCode")
    if has_permanent and has_status_code:
        fail(f"redirect entry {index} must not set both permanent and statusCode")
    if has_permanent and not isinstance(item["permanent"], bool):
        fail(f"redirect entry {index} permanent must be boolean")
    if has_status_code:
        status_code = item["statusCode"]
        if not isinstance(status_code, int) or isinstance(status_code, bool):
            fail(f"redirect entry {index} statusCode must be an integer")
        if status_code < 100 or status_code > 999:
            fail(f"redirect entry {index} statusCode must be between 100 and 999")


def validate_redirect_env(index: int, item: dict[str, object]) -> None:
    if "env" not in item:
        return

    env = item["env"]
    if not isinstance(env, list):
        fail(f"redirect entry {index} env must be a list")
    if not env:
        fail(f"redirect entry {index} env must not be empty")
    if len(env) > MAX_ENV_ITEMS:
        fail(f"redirect entry {index} env must contain at most {MAX_ENV_ITEMS} items")
    for env_index, value in enumerate(env):
        if not isinstance(value, str):
            fail(f"redirect entry {index} env[{env_index}] must be a string")
        if len(value) > MAX_ENV_VALUE_LENGTH:
            fail(
                f"redirect entry {index} env[{env_index}] "
                f"must be at most {MAX_ENV_VALUE_LENGTH} characters"
            )


def collect_redirects(data: dict[str, object]) -> dict[str, dict[str, object]]:
    raw_redirects = data.get("redirects", [])
    if not isinstance(raw_redirects, list):
        fail("vercel redirects must be a list")
    if len(raw_redirects) > MAX_REDIRECTS:
        fail(f"vercel redirects must contain at most {MAX_REDIRECTS} items")

    redirects: dict[str, dict[str, object]] = {}
    for index, item in enumerate(raw_redirects):
        if not isinstance(item, dict):
            fail(f"redirect entry {index} must be an object")
        unknown_keys = set(item) - ALLOWED_REDIRECT_KEYS
        if unknown_keys:
            keys = ", ".join(sorted(unknown_keys))
            fail(f"redirect entry {index} has unknown keys: {keys}")
        source = item.get("source")
        if not isinstance(source, str) or not source:
            fail(f"redirect entry {index} missing source")
        validate_schema_string(f"redirect entry {index} source", source)
        destination = item.get("destination")
        if not isinstance(destination, str) or not destination:
            fail(f"redirect entry {index} missing destination")
        validate_schema_string(f"redirect entry {index} destination", destination)
        validate_redirect_mode(index, item)
        validate_redirect_env(index, item)
        validate_condition_list(index, item, "has")
        validate_condition_list(index, item, "missing")
        if source in redirects:
            fail(f"duplicate redirect source {source}")
        redirects[source] = item
    return redirects


def load_vercel() -> dict[str, Any]:
    try:
        data = json.loads(
            VERCEL.read_text(encoding="utf-8"),
            parse_constant=reject_json_constant,
        )
    except FileNotFoundError:
        fail("missing vercel.json")
    except json.JSONDecodeError as error:
        fail(f"invalid vercel.json: {error}")
    except ValueError as error:
        fail(f"invalid vercel.json: {error}")

    if not isinstance(data, dict):
        fail("vercel.json must contain an object")
    validate_finite_json_numbers("vercel.json", data)
    return data


def main() -> None:
    data = load_vercel()
    if "framework" not in data or data["framework"] is not None:
        fail("vercel framework must be null")
    if data.get("installCommand") != "uv sync --frozen --no-dev":
        fail("unexpected Vercel installCommand")
    if data.get("buildCommand") != "uv run --frozen bash ./vercel-build.sh":
        fail("unexpected Vercel buildCommand")
    if data.get("outputDirectory") != "site":
        fail("unexpected Vercel outputDirectory")
    if data.get("trailingSlash") is not True:
        fail("vercel.json must set trailingSlash true")

    redirects = collect_redirects(data)
    for source, destination in PERMANENT.items():
        item = redirects.get(source)
        if not item:
            fail(f"missing permanent redirect {source}")
        if item.get("destination") != destination or item.get("permanent") is not True:
            fail(f"incorrect permanent redirect {source}")

    for source, destination in TEMPORARY.items():
        item = redirects.get(source)
        if not item:
            fail(f"missing temporary redirect {source}")
        if item.get("destination") != destination or item.get("permanent") is not False:
            fail(f"incorrect temporary redirect {source}")

    print("vercel redirect checks passed")


if __name__ == "__main__":
    main()
