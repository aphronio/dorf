#!/usr/bin/env python3
"""Normalize a Nostr npub or hex public key to 64 lowercase hex characters."""

from __future__ import annotations

import re
import sys

BECH32_ALPHABET = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
BECH32_VALUES = {character: index for index, character in enumerate(BECH32_ALPHABET)}
HEX_PUBLIC_KEY = re.compile(r"[0-9a-fA-F]{64}")


def _bech32_polymod(values: list[int]) -> int:
    checksum = 1
    generators = (0x3B6A57B2, 0x26508E6D, 0x1EA119FA, 0x3D4233DD, 0x2A1462B3)
    for value in values:
        top = checksum >> 25
        checksum = ((checksum & 0x1FFFFFF) << 5) ^ value
        for bit, generator in enumerate(generators):
            if (top >> bit) & 1:
                checksum ^= generator
    return checksum


def _expand_hrp(hrp: str) -> list[int]:
    return [ord(character) >> 5 for character in hrp] + [0] + [
        ord(character) & 31 for character in hrp
    ]


def _convert_bits(values: list[int], from_bits: int, to_bits: int) -> bytes:
    accumulator = 0
    bit_count = 0
    result = bytearray()
    maximum_value = (1 << to_bits) - 1

    for value in values:
        if value < 0 or value >> from_bits:
            raise ValueError("npub contains an invalid data value")
        accumulator = (accumulator << from_bits) | value
        bit_count += from_bits
        while bit_count >= to_bits:
            bit_count -= to_bits
            result.append((accumulator >> bit_count) & maximum_value)

    if bit_count >= from_bits or ((accumulator << (to_bits - bit_count)) & maximum_value):
        raise ValueError("npub has invalid padding")
    return bytes(result)


def normalize_public_key(value: str) -> str:
    candidate = value.strip()
    if HEX_PUBLIC_KEY.fullmatch(candidate):
        return candidate.lower()

    if candidate.lower() != candidate and candidate.upper() != candidate:
        raise ValueError("npub must not mix uppercase and lowercase characters")

    encoded = candidate.lower()
    separator = encoded.rfind("1")
    if separator <= 0 or separator + 7 > len(encoded):
        raise ValueError("expected an npub or 64-character hex public key")

    hrp = encoded[:separator]
    if hrp != "npub":
        raise ValueError("expected an npub public key")

    try:
        data = [BECH32_VALUES[character] for character in encoded[separator + 1 :]]
    except KeyError as error:
        raise ValueError("npub contains an invalid bech32 character") from error

    if _bech32_polymod(_expand_hrp(hrp) + data) != 1:
        raise ValueError("npub checksum is invalid")

    decoded = _convert_bits(data[:-6], 5, 8)
    if len(decoded) != 32:
        raise ValueError("npub must encode exactly 32 bytes")
    return decoded.hex()


def main(arguments: list[str]) -> int:
    if len(arguments) != 1:
        print(
            "usage: normalize-nostr-public-key.py <npub-or-64-char-hex>",
            file=sys.stderr,
        )
        return 2

    try:
        print(normalize_public_key(arguments[0]))
    except ValueError as error:
        print(f"Invalid Buzz owner public key: {error}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
