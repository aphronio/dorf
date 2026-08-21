# Releasing Dorf

From a clean source commit already available on GitHub, run the repository checks and the release
authority:

```bash
scripts/dev/setup.sh
.dorf/bin/mise run check

AI_CONNECTION=personal-chatgpt \
GITHUB_INSTALLATION_ID=INSTALLATION_ID \
  scripts/incus/release-dorf-image.sh --publish
```

[`scripts/incus/release-dorf-image.sh`](../scripts/incus/release-dorf-image.sh) is the
source of truth for release inputs, proof gates, artifacts, and publication. Do not duplicate those
details here or publish by bypassing that command.
