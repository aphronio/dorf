# Dorf Incus Image

Dorf's official Sandbox image is a credential-free Incus VM. Managed repositories own their project
dependencies and deterministic setup.

- Shared guest construction authority: [`scripts/sandbox/provision-dorf-guest.sh`](../../scripts/sandbox/provision-dorf-guest.sh)
- Incus packaging authority: [`scripts/incus/build-dorf-image.sh`](../../scripts/incus/build-dorf-image.sh)
- Candidate proof and publication authority: [`scripts/incus/release-dorf-image.sh`](../../scripts/incus/release-dorf-image.sh)
- Manifest validation and installation contract: [`internal/release/image.go`](../../internal/release/image.go)
- Operator release commands: [`docs/releasing.md`](../releasing.md)
- Product boundary and rationale: [`docs/project/decisions.md`](../project/decisions.md)
- Provider credential boundary: [`docs/project/provider-gateway.md`](../project/provider-gateway.md)

Exact guest tool and Harness versions live in the shared recipe. Incus base identity, packaging,
manifest validation, and release steps live in their provider-specific authorities rather than this
pointer page.
