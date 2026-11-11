// Runs the named proofs at once, each with its own agent, port and stand-in gate, printing each one whole as it ends.
import { spawn } from "node:child_process";

const names = process.argv.slice(2);
const passed = await Promise.all(
	names.map(
		(name) =>
			new Promise<boolean>((resolve) => {
				const proof = spawn("node", [new URL(`./${name}.ts`, import.meta.url).pathname], { stdio: ["ignore", "pipe", "pipe"] });
				let output = "";
				proof.stdout.on("data", (d) => (output += d));
				proof.stderr.on("data", (d) => (output += d));
				proof.on("close", (code) => {
					process.stdout.write(`\n== ${name}\n${output}`);
					resolve(code === 0);
				});
			}),
	),
);
const failed = names.filter((_, i) => !passed[i]);
if (failed.length) {
	console.error(`\nFAIL: ${failed.join(", ")}`);
	process.exit(1);
}
console.log(`\nPASS: ${names.join(", ")}`);
