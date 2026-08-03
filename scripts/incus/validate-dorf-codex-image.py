#!/usr/bin/env python3
"""Run one real disposable Worker turn against a candidate Room image."""

from __future__ import annotations

import argparse
import secrets
import tempfile
from pathlib import Path

from dorf import Dorf
from dorf.adapters.environments import IncusConfig, incus_bridge_ipv4
from dorf.provider_gateway import ProviderGateway

EXPECTED_RESPONSE = "dorf image ready"


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--image", required=True)
    parser.add_argument("--provider-connection", required=True)
    parser.add_argument("--network", default="incusbr0")
    parser.add_argument("--root-disk-size", default="40GiB")
    args = parser.parse_args()

    suffix = secrets.token_hex(4)
    worker_name = f"image-validation-{suffix}"
    config = IncusConfig(
        template=args.image,
        network=args.network,
        root_disk_size=args.root_disk_size,
    )
    with tempfile.TemporaryDirectory(prefix="dorf-image-validation.") as directory:
        database_path = Path(directory) / "runtime.db"
        with ProviderGateway.open(bind_address=incus_bridge_ipv4(config.network)) as gateway:
            with Dorf.open(
                database_path,
                environment_config=config,
                provider_connection=args.provider_connection,
                provider_gateway=gateway,
            ) as dorf:
                binding = None
                try:
                    binding = dorf.spawn_worker(worker_name)
                    receipt = dorf.message_worker(
                        worker_name,
                        f"Reply with exactly: {EXPECTED_RESPONSE}",
                    )
                    result = dorf.wait_for_worker_message(
                        worker_name,
                        message_id=receipt.message.id,
                        timeout=180,
                    )
                    if result.outcome != "done" or result.response != EXPECTED_RESPONSE:
                        raise RuntimeError(
                            "candidate image did not complete the expected real Codex turn"
                        )
                    ended = dorf.end_worker(worker_name)
                    if ended.worker.status != "ended" or ended.room is None:
                        raise RuntimeError("candidate image validation Room was not destroyed")
                    consumer = f"room:{binding.room.id}"
                    if gateway.route_for_consumer(consumer) is not None:
                        raise RuntimeError("candidate image validation route was not revoked")
                finally:
                    worker = dorf.get_worker(worker_name)
                    if worker is not None and worker.status != "ended":
                        dorf.end_worker(worker_name, interrupt=True)

    print(f"Validated Codex {EXPECTED_RESPONSE!r} through {args.image}")


if __name__ == "__main__":
    main()
