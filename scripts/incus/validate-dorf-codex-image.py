#!/usr/bin/env python3
"""Run one real disposable Worker turn against a candidate Room image."""

from __future__ import annotations

import argparse

from dorf.adapters.environments import incus_bridge_ipv4
from dorf.provider_gateway import ProviderGateway, ProviderGatewayError


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--image")
    parser.add_argument("--provider-connection", required=True)
    parser.add_argument("--network", default="incusbr0")
    parser.add_argument("--root-disk-size", default="40GiB")
    parser.add_argument("--preflight-only", action="store_true")
    args = parser.parse_args()

    try:
        with ProviderGateway.open(bind_address=incus_bridge_ipv4(args.network)) as gateway:
            gateway.require_connection(args.provider_connection)
    except (ProviderGatewayError, RuntimeError) as error:
        raise SystemExit(
            f"Provider release preflight failed: {error}. "
            "Run from the Dorf source checkout: "
            f"go run ./cmd/dorf doctor --provider {args.provider_connection}"
        ) from None
    if not args.preflight_only:
        parser.error(
            "candidate turns are validated by validate-dorf-coding-workstation.py through Go"
        )
    print(f"Provider release preflight passed: {args.provider_connection}")


if __name__ == "__main__":
    main()
