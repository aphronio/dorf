// Disposable image builds and repeatable verification. Bun loads local credentials privately.
import { createHash, randomBytes } from "node:crypto";
import { appendFile, mkdir, readFile, writeFile } from "node:fs/promises";
import { resolve } from "node:path";
import { packageInputs } from "../sandbox/package-inputs";

const [operation, provider, receiptPath] = process.argv.slice(2);
if (!["build", "verify"].includes(operation) || !["incus", "e2b"].includes(provider)) {
  throw new Error("usage: integration:nix-image build|verify incus|e2b [build receipt]");
}
if (operation === "verify" && !receiptPath) throw new Error("verify requires the exact build receipt");
const root = resolve(import.meta.dir, "../..");
const inputs = ["scripts/sandbox/provision-dorf-guest.sh", "scripts/sandbox/package-inputs.ts",
  ...(provider === "incus" ? ["scripts/incus/build-dorf-image.sh"] : ["scripts/e2b/build-template.ts"])];
const hashes = await packageInputs(root);
for (const path of inputs) hashes[path] = createHash("sha256").update(await readFile(resolve(root, path))).digest("hex");
const receipt = operation === "verify" ? JSON.parse(await readFile(resolve(receiptPath), "utf8")) : {
  id: `upgrade-proof-${randomBytes(6).toString("hex")}`, provider, inputs: hashes,
};
if (receipt.provider !== provider || JSON.stringify(receipt.inputs) !== JSON.stringify(hashes)) {
  throw new Error("build receipt does not match this provider and checked-out image inputs");
}
if (!/^upgrade-proof-[a-f0-9]{12}$/.test(receipt.id)) throw new Error("invalid image proof ID");
const evidence = resolve(root, ".dorf/runtime-upgrade", receipt.id);
await mkdir(evidence, { recursive: true, mode: 0o700 });
const manifest = resolve(evidence, "profile.json");
const env = { ...process.env, GOFLAGS: "-p=1", DORF_ALLOW_DIRTY_TEMPLATE_BUILD: "1" };
async function run(argv: string[], extra: Record<string, string> = {}, capture = false) {
  const child = Bun.spawn(argv, { cwd: root, env: { ...env, ...extra }, stdin: "ignore", stdout: capture ? "pipe" : "inherit", stderr: "inherit" });
  const output = capture ? await new Response(child.stdout).text() : "";
  if (await child.exited !== 0) throw new Error(`${argv[0]} failed; inspect the command output`);
  return output.trim();
}
async function event(phase: string, detail: Record<string, unknown> = {}) {
  const entry = { upgrade_id: receipt.id, provider, phase, at: new Date().toISOString(), ...detail };
  await appendFile(resolve(evidence, "events.jsonl"), JSON.stringify(entry) + "\n");
  console.log(JSON.stringify(entry));
}
await event(`image-${operation}.started`);
try {
  if (operation === "build") {
    receipt.source_commit = await run(["git", "rev-parse", "HEAD"], {}, true);
    receipt.source_dirty = (await run(["git", "status", "--porcelain"], {}, true)) !== "";
    await writeFile(resolve(evidence, "build.json"), JSON.stringify(receipt, null, 2) + "\n");
    if (provider === "incus") {
      receipt.alias = `dorf-nix-${receipt.id}`;
      await run(["scripts/incus/build-dorf-image.sh"], {
        IMAGE_ALIAS: receipt.alias, BUILD_VM: `${receipt.alias}-build`,
        IMAGE_METADATA_PATH: resolve(evidence, "image.json"), ROOT_DISK_SIZE: "50GiB",
      });
      const info = JSON.parse(await run(["incus", "image", "list", receipt.alias, "--format=json"], {}, true));
      if (info.length !== 1) throw new Error("candidate image identity is ambiguous");
      receipt.image_fingerprint = info[0].fingerprint;
      await writeFile(manifest, JSON.stringify({ image_fingerprint: receipt.image_fingerprint }) + "\n");
    } else {
      await run(["scripts/e2b/build-template.sh"], {
        DORF_E2B_TEMPLATE_NAME: `dorf-nix-${receipt.id}`,
        DORF_E2B_TEMPLATE_MANIFEST: manifest,
      });
      receipt.template = JSON.parse(await readFile(manifest, "utf8")).template.reference;
    }
    await writeFile(resolve(evidence, "build.json"), JSON.stringify(receipt, null, 2) + "\n");
    await event("image-build.completed", { source_commit: receipt.source_commit, source_dirty: receipt.source_dirty,
      image_fingerprint: receipt.image_fingerprint, template: receipt.template });
    console.log(`Verify: mise run integration:nix-image verify ${provider} ${resolve(evidence, "build.json")}`);
  } else {
    const artifact = JSON.parse(await readFile(manifest, "utf8"));
    if (provider === "incus" ? !receipt.image_fingerprint || artifact.image_fingerprint !== receipt.image_fingerprint
      : !receipt.template || artifact.template.reference !== receipt.template) {
      throw new Error("artifact no longer matches the exact completed build receipt");
    }
    if (provider === "e2b") {
      await run(["mise", "exec", "--", "go", "test", "./internal/e2b", "-run", "^TestLiveCombinedHarnessProfile$", "-count=1", "-v"], {
        DORF_E2B_PROFILE_LIVE: "1", DORF_E2B_PROFILE_MANIFEST: manifest,
      });
    }
    await run(["mise", "run", "integration:upgrade-worker", provider], {
      DORF_UPGRADE_REQUIRE_NIX_IMAGE: "1", DORF_UPGRADE_INCUS_MANIFEST: manifest,
      DORF_UPGRADE_INCUS_PROJECT: "default", DORF_E2B_PROFILE_MANIFEST: manifest,
    });
    await event("image-verify.completed", { image_fingerprint: receipt.image_fingerprint, template: receipt.template });
  }
} catch (error) {
  await event(`image-${operation}.failed`);
  throw error;
}
