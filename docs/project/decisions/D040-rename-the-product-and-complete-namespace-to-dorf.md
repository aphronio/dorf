# D040: Rename the product and complete namespace to Dorf

- **Applicability:** partial
- **Areas:** product, release, deployment
- **Read when:** Changing the Dorf name, command, namespace, configuration paths, or artifact identity.
- **Decision history:** Accepted for initial open-source distribution — 2026-08-03; Python distribution
  portion superseded by D047 — 2026-08-08
- **Decision:** Rename the product to Dorf and use `dorf` consistently for the Go application,
  CLI command, repository contract, local configuration and state paths,
  environment-variable prefix, image and release artifacts, and repository identity. Do not retain
  compatibility aliases or migrate private dogfood state from the former pre-release namespace.
- **Why:** The selected identity should remain coherent across the installed artifact and every
  user-facing surface. The cutover has no old package compatibility obligation.
- **Compatibility:** This intentionally breaks private source imports, commands, configuration,
  state paths, environment variables, image names, and integrations that used the former namespace.
  Existing dogfood resources must be ended or recreated explicitly; Dorf does not guess ownership
  of or mutate the old namespace. The `dorf` identity becomes a public compatibility concern only
  after the first release.
- **Reconsider when:** A credible developer-tool naming conflict is discovered. After publication,
  require a deliberate migration decision.
