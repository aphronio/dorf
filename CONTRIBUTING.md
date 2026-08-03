# Contributing to Dorf

Thanks for your interest in contributing. Dorf is licensed under the
[Apache License 2.0](LICENSE).

## Development setup

```bash
uv run dorf --help
uv run pytest
uv run ruff check .
```

Run the tests and linter before opening a pull request.

## DCO sign-off

Dorf uses the [Developer Certificate of Origin](https://developercertificate.org/)
(DCO) instead of a Contributor License Agreement. By adding a `Signed-off-by` line to
each commit, you certify that you wrote the contribution or otherwise have the right
to submit it under the project's Apache 2.0 license.

Sign off every commit with the `-s` flag:

```bash
git commit -s -m "your message"
```

This appends a line like:

```
Signed-off-by: Jane Doe <jane@example.com>
```

Use your real name and a reachable email address (the identity from `git config
user.name` / `user.email`). Pull requests with commits missing the sign-off cannot be
merged; the DCO status check will fail. If you forgot to sign off, fix existing commits
with:

```bash
git rebase --signoff main
git push --force-with-lease
```

## Licensing of contributions

Contributions are submitted under the same Apache 2.0 license, per the DCO and
section 5 of the Apache License. You retain the copyright on your contributions.

## Pull requests

- Open an issue first for anything larger than a small fix.
- Keep PRs focused: one concern per PR.
- Make sure `uv run pytest` and `uv run ruff check .` pass before pushing.
