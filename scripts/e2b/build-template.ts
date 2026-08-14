import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { Template, defaultBuildLogger } from "e2b";

const sdkVersion = "2.39.0";
const templateName = process.env.DORF_E2B_TEMPLATE_NAME ?? "dorf-debian13-combined";
const baseDigest = "d8f17b92dc7ff10f9c1fdecab0ad21103d1d24aed823c3a0359e4f50adfab3eb";
const baseReference = `debian@sha256:${baseDigest}`;
const scriptDirectory = dirname(fileURLToPath(import.meta.url));
const projectRoot = resolve(scriptDirectory, "../..");
const recipeRelativePath = "scripts/sandbox/provision-dorf-guest.sh";
const recipePath = resolve(projectRoot, recipeRelativePath);
const manifestPath = resolve(
  projectRoot,
  process.env.DORF_E2B_TEMPLATE_MANIFEST ?? "dist/e2b-template/profile.json",
);

function git(...args: string[]): string {
  return execFileSync("git", ["-C", projectRoot, ...args], { encoding: "utf8" }).trim();
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", `'"'"'`)}'`;
}

async function main() {
  if (!process.env.E2B_API_KEY) throw new Error("E2B_API_KEY is required");

  const sourceCommit = git("rev-parse", "HEAD");
  const dirty = git("status", "--porcelain", "--untracked-files=all") !== "";
  if (dirty && process.env.DORF_ALLOW_DIRTY_TEMPLATE_BUILD !== "1") {
    throw new Error(
      "E2B template construction requires a clean source tree; commit the exact recipe first",
    );
  }

  const recipe = await readFile(recipePath);
  const recipeSHA256 = createHash("sha256").update(recipe).digest("hex");
  const provision = [
    `DORF_BASE_IMAGE=${shellQuote(baseReference)}`,
    `DORF_BASE_FINGERPRINT=${shellQuote(baseDigest)}`,
    "/tmp/provision-dorf-guest.sh",
  ].join(" ");

  const template = Template({ fileContextPath: projectRoot })
    .fromImage(baseReference)
    .copy(recipeRelativePath, "/tmp/provision-dorf-guest.sh", {
      forceUpload: true,
      mode: 0o755,
      user: "root",
    })
    .runCmd(provision, { user: "root" })
    .remove("/tmp/provision-dorf-guest.sh", { force: true, user: "root" })
    .setUser("root")
    .setWorkdir("/workspace/job");

  const built = await Template.build(template, templateName, {
    cpuCount: 4,
    memoryMB: 4096,
    onBuildLogs: defaultBuildLogger({ minLevel: "info" }),
  });
  const exactReference = `${built.name}:${built.buildId}`;
  const manifest = {
    schema_version: 1,
    provider: "e2b",
    sdk_version: sdkVersion,
    template: {
      name: built.name,
      id: built.templateId,
      build_id: built.buildId,
      reference: exactReference,
    },
    base_image: {
      reference: baseReference,
      digest: `sha256:${baseDigest}`,
      platform: "linux/amd64",
    },
    profile: {
      recipe: recipeRelativePath,
      recipe_sha256: recipeSHA256,
      metadata_path: "/usr/local/share/dorf/image.json",
      workspace: "/workspace/job",
      default_user: "root",
    },
    source_commit: sourceCommit,
    source_dirty: dirty,
    built_at: new Date().toISOString(),
  };

  await mkdir(dirname(manifestPath), { recursive: true });
  await writeFile(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`, { mode: 0o644 });
  console.log(`Built exact E2B template ${exactReference}`);
  console.log(`Wrote ${manifestPath}`);
}

await main();
