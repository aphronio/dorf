# Dorf Incus Image

Dorf's official Sandbox image is a credential-free Incus VM. Managed repositories own their project
dependencies and deterministic setup.

- Image construction authority: [`scripts/incus/build-dorf-image.sh`](../../scripts/incus/build-dorf-image.sh)
- Candidate proof and publication authority: [`scripts/incus/release-dorf-image.sh`](../../scripts/incus/release-dorf-image.sh)
- Manifest validation and installation contract: [`internal/release/image.go`](../../internal/release/image.go)
- Operator release commands: [`docs/releasing.md`](../releasing.md)
- Product boundary and rationale: [`docs/project/decisions.md`](../project/decisions.md)
- Provider credential boundary: [`docs/project/provider-gateway.md`](../project/provider-gateway.md)

Exact base images, tool versions, inventory, integrity checks, and release steps intentionally live
in those authorities rather than this pointer page.
