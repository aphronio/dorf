# E2B envd process schema

`process/process.proto` is vendored from E2B infra commit with trailing whitespace removed
`d586e2e86853b19b156fe7bc1cd76c2ea0b45475`:

<https://github.com/e2b-dev/infra/blob/d586e2e86853b19b156fe7bc1cd76c2ea0b45475/packages/envd/spec/process/process.proto>

E2B infra and this repository are licensed under Apache-2.0. Generated Go files are committed under
`internal/e2b/gen`. Generation pins protoc 35.1, protoc-gen-go 1.36.11, and
protoc-gen-connect-go 1.18.1. Regenerate from the repository root with:

```bash
scripts/e2b/generate-envd.sh
```

Update the source, commit SHA, plugin versions, generated files, and envd compatibility tests in one
reviewed change. Do not generate from E2B's moving `main` branch.
