# Releasing Dorf to Python package indexes

Dorf publishes verified Python distributions from GitHub Actions with PyPI Trusted Publishing.
Maintainers do not create or store long-lived PyPI API tokens.

## One-time account and repository setup

1. Create separate accounts on [PyPI](https://pypi.org/account/register/) and
   [TestPyPI](https://test.pypi.org/account/register/). Verify each primary email, enable two-factor
   authentication, and store the recovery codes securely.
2. Rename the GitHub repository to `aphronio/dorf` and make it public.
3. Create GitHub environments named `testpypi` and `pypi`. Protect the production environment as
   appropriate for the repository's maintainer model.
4. On TestPyPI, register a pending Trusted Publisher with project `dorf`, owner `aphronio`,
   repository `dorf`, workflow `publish-python.yml`, and environment `testpypi`.
5. On PyPI, register the same pending publisher with environment `pypi`.

A pending Trusted Publisher can create the project on first publication, but it does not reserve
the project name. Do not create the project or an API token manually.

## Rehearsal and release

Before tagging, run the local package gate:

```bash
scripts/verify-python-package.sh dist
```

The script builds the wheel and source distribution, applies strict metadata checks, and
clean-installs both artifacts.

Run the `Publish Python package` workflow manually to publish the current revision to TestPyPI.
Install and exercise that exact TestPyPI version before production publication.

For production, set the version in `pyproject.toml` and `src/dorf/__init__.py`, merge the release
commit, and publish a GitHub release whose tag is exactly `v` followed by that version, such as
`v0.1.0`. The workflow rejects a release tag that does not match the package metadata. A GitHub
release with any other tag prefix, including a Room-image release, cannot publish to PyPI.

See PyPI's documentation for
[Trusted Publishing from GitHub Actions](https://docs.pypi.org/trusted-publishers/using-a-publisher/)
and [creating a project through OIDC](https://docs.pypi.org/trusted-publishers/creating-a-project-through-oidc/).
