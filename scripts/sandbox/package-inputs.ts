import { createHash } from "node:crypto";
import { readdir, readFile } from "node:fs/promises";
import { resolve } from "node:path";

export async function packageInputs(root: string): Promise<Record<string, string>> {
  const hashes: Record<string, string> = {};
  async function visit(directory: string) {
    const entries = await readdir(resolve(root, directory), { withFileTypes: true });
    for (const entry of entries.sort((a, b) => a.name.localeCompare(b.name))) {
      const path = `${directory}/${entry.name}`;
      if (entry.isDirectory()) await visit(path);
      else if (entry.isFile()) hashes[path] = createHash("sha256").update(await readFile(resolve(root, path))).digest("hex");
      else throw new Error(`Package input must be a regular file: ${path}`);
    }
  }
  await visit("scripts/sandbox/packages");
  return hashes;
}
