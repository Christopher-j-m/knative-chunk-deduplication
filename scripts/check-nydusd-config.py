#!/usr/bin/env python3
"""Check that a nydusd configuration reads from the expected place.

Reads the configuration from standard input and compares its storage backend,
the number of registry mirrors and the prefetch mode with what the caller
expects. Prints one line describing the configuration and exits non-zero when
it differs, so a configuration switch can refuse to hand over a node that would
silently measure another system. Run with:
check-nydusd-config.py <http-proxy|registry> <mirror count> <off|all> < nydusd-config.json
"""

import json
import sys


# Reduces a configuration to the three facts that decide where chunks come
# from and whether the whole image is fetched ahead of the first read
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
