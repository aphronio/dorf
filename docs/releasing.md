# Releasing Dorf

From a clean source commit already available on GitHub, run the repository checks and the release
authority:

```bash
scripts/dev/setup.sh
.dorf/bin/mise run check
scripts/release.sh --publish
```

[`scripts/release.sh`](../scripts/release.sh) is the source of truth for release inputs, artifacts,
and publication. It reuses the exact pinned Incus image unless that image's declared inputs changed.
When the pin advances to the release being published, set `AI_CONNECTION` and
`GITHUB_INSTALLATION_ID`; the authority then invokes the real Codex and Pi image proof before
publication. Do not duplicate those details here or publish by bypassing that command.
