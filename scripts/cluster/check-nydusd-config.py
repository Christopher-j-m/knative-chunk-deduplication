#!/usr/bin/env python3
"""Checks that a nydusd configuration uses the expected storage backend, number of mirrors and prefetch mode"""
import json
import sys


def describe(config):
    backend = config["device"]["backend"]
    mirrors = len(backend.get("config", {}).get("mirrors", []))
    prefetch = config.get("fs_prefetch", {})
    enabled = bool(prefetch.get("enable")) and bool(prefetch.get("prefetch_all"))
    disabled = not prefetch.get("enable") and not prefetch.get("prefetch_all")
    if enabled:
        mode = "all"
    elif disabled:
        mode = "off"
    else:
        mode = "partial"
    return backend["type"], mirrors, mode


def main():
    if len(sys.argv) != 4:
        sys.exit("usage: check-nydusd-config.py <http-proxy|registry> <mirror count> <off|all>")
    expected = (sys.argv[1], int(sys.argv[2]), sys.argv[3])

    try:
        config = json.load(sys.stdin)
        actual = describe(config)
    except (ValueError, KeyError, TypeError) as error:
        print(f"unreadable nydusd configuration: {error}")
        sys.exit(1)

    line = f"backend {actual[0]}, {actual[1]} mirrors, prefetch {actual[2]}"
    if actual != expected:
        print(f"{line}; expected backend {expected[0]}, {expected[1]} mirrors, prefetch {expected[2]}")
        sys.exit(1)
    print(line)


if __name__ == "__main__":
    main()
