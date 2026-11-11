import { randomBytes } from "node:crypto";
import { crc32, deflateSync } from "node:zlib";
import { call, fail, killAgent, log, pass, startAgent, until } from "./kit.ts";

type Block = { type: string; text?: string; data?: string; mimeType?: string };
type Entry = { id: number; kind: string; model?: { role: string; content: string | Block[] }[] };

// A PNG of the given size: one colour, or noise that does not compress.
function png(width: number, height: number, noise = false): string {
	const chunk = (type: string, data: Buffer) => {
		const body = Buffer.concat([Buffer.from(type, "latin1"), data]);
		const out = Buffer.alloc(body.length + 8);
		out.writeUInt32BE(data.length, 0);
		body.copy(out, 4);
		out.writeUInt32BE(crc32(body), body.length + 4);
		return out;
	};
	const header = Buffer.alloc(13);
	header.writeUInt32BE(width, 0);
	header.writeUInt32BE(height, 4);
	header.set([8, 2, 0, 0, 0], 8);
	const row = width * 3 + 1;
	const pixels = noise ? randomBytes(row * height) : Buffer.alloc(row * height, 0x80);
	for (let y = 0; y < height; y++) pixels[y * row] = 0;
	return Buffer.concat([Buffer.from("\x89PNG\r\n\x1a\n", "latin1"), chunk("IHDR", header), chunk("IDAT", deflateSync(pixels, { level: 1 })), chunk("IEND", Buffer.alloc(0))]).toString("base64");
}
const widthOf = (data: string) => Buffer.from(data, "base64").readUInt32BE(16);

const blocks = (e: Entry): Block[] => {
	const content = e.model?.[0]?.content ?? [];
	return typeof content === "string" ? [{ type: "text", text: content }] : content;
};
const textOf = (e: Entry) => blocks(e).flatMap((b) => (b.type === "text" ? [b.text] : [])).join("");
const entries = async (id: number): Promise<Entry[]> => (await call("GET", `/threads/${id}`)).json.entries;

// The newest user message once the faux model has answered it: it says how many images it saw.
async function answered(id: number, reply: string): Promise<Entry> {
	return until(`the reply "${reply}"`, async () => {
		const { json } = await call("GET", `/threads/${id}`);
		if (json.docs["pi.live"]?.run) return undefined;
		const list: Entry[] = json.entries;
		const asked = list.findLastIndex((e) => e.kind === "pi.user");
		return list.slice(asked).some((e) => e.kind === "pi.assistant" && textOf(e) === reply) && list[asked];
	});
}

await startAgent();

const created = await call("POST", "/threads", { prompt: "what is in this picture?", images: [png(3000, 1000)] });
if (created.status !== 201) await fail(`create: ${created.status} ${JSON.stringify(created.json)}`);
const { id } = created.json;
const first = blocks(await answered(id, "done, saw 1 image"));
const [question, picture] = first;
if (question?.text !== "what is in this picture?" || picture?.type !== "image" || picture.mimeType !== "image/png" || widthOf(picture.data!) > 2000)
	await fail(`first message: ${JSON.stringify(first.map((b) => ({ ...b, data: b.data && `${b.data.length} chars` })))}`);
log("1. a Thread starts from text and an image; the model sees the image, shrunk to the model's 2000 px", `width ${widthOf(picture!.data!)}`);

if ((await call("POST", `/threads/${id}/messages`, { images: [png(40, 30), png(30, 40)] })).status !== 202) await fail("image-only send");
const only = blocks(await answered(id, "done, saw 2 images"));
if (only.length !== 2 || only.some((b) => b.type !== "image")) await fail(`image-only message: ${JSON.stringify(only.map((b) => b.type))}`);
log("2. a message of only images reaches the model with no empty text block");

if ((await call("POST", `/threads/${id}/messages`, { text: "$ sleep 3" })).status !== 202) await fail("send sleep");
await until("the sleep running", async () => (await call("GET", `/threads/${id}`)).json.docs["pi.live"]?.tools?.length > 0);
if ((await call("POST", `/threads/${id}/messages`, { text: "and this one", images: [png(50, 50)] })).status !== 202) await fail("steer");
const steer = await answered(id, "done, saw 1 image");
const list = await entries(id);
const sleep = list.findLastIndex((e) => e.kind === "pi.user" && textOf(e) === "$ sleep 3");
const placed = list.findIndex((e) => e.id === steer.id);
if (textOf(steer) !== "and this one" || !list.slice(sleep, placed).some((e) => e.kind === "pi.tool-result")) await fail(`steer: ${textOf(steer)} at ${placed}, sleep at ${sleep}`);
log("3. an image sent mid-run steers the run, after its tool call");

// Two PNGs made in the Sandbox: one wider than the model takes, one whose header claims 400 megapixels in 70 KB.
const make = `node -e 'const z=require("zlib"),fs=require("fs"),c=(t,d)=>{const b=Buffer.concat([Buffer.from(t),d]),o=Buffer.alloc(b.length+8);o.writeUInt32BE(d.length);b.copy(o,4);o.writeUInt32BE(z.crc32(b),b.length+4);return o},png=(w,h,depth,type)=>{const H=Buffer.alloc(13);H.writeUInt32BE(w);H.writeUInt32BE(h,4);H[8]=depth;H[9]=type;const row=Math.ceil(w*depth*(type==2?3:1)/8)+1;return Buffer.concat([Buffer.from("89504e470d0a1a0a","hex"),c("IHDR",H),c("IDAT",z.deflateSync(Buffer.alloc(row*h))),c("IEND",Buffer.alloc(0))])};fs.writeFileSync("wide.png",png(3000,1000,8,2));fs.writeFileSync("huge.png",png(20000,20000,1,0))'`;
if ((await call("POST", `/threads/${id}/messages`, { text: `$ ${make}\n> wide.png\n> huge.png` })).status !== 202) await fail("send read");
await answered(id, "done");
const reads = (await entries(id)).filter((e) => e.kind === "pi.tool-result").slice(-2).map((e) => ({ blocks: blocks(e), error: Boolean((e.model?.[0] as { isError?: boolean })?.isError) }));
const [wide, huge] = reads;
const read = wide?.blocks.find((b) => b.type === "image");
if (!read || widthOf(read.data!) > 2000) await fail(`read wide.png: ${JSON.stringify(wide?.blocks.map((b) => b.type))}`);
if (!huge?.error || !huge.blocks.some((b) => b.text?.includes("cannot be prepared"))) await fail(`read huge.png: ${JSON.stringify(huge)}`);
log("4. read shrinks a wide PNG the same way, and refuses one claiming 400 megapixels before decoding it", `width ${widthOf(read!.data!)}`);

const before = (await entries(id)).filter((e) => e.kind === "pi.user").map((e) => JSON.stringify(blocks(e)));
killAgent("SIGKILL");
await startAgent();
const after = (await entries(id)).filter((e) => e.kind === "pi.user").map((e) => JSON.stringify(blocks(e)));
if (after.join() !== before.join()) await fail("the images changed across a restart");
log("5. after the agent is killed and restarted, every message keeps its images");

const threads = (await call("GET", "/threads")).json.length;
const refused: [string, unknown, number, string][] = [
	["no text and no images", { prompt: "" }, 400, "prompt or images required"],
	["not base64", { prompt: "x", images: ["not base64!"] }, 400, "not base64"],
	["not an image", { prompt: "x", images: [Buffer.from("hello").toString("base64")] }, 400, "not a PNG"],
	["a corrupt PNG", { prompt: "x", images: [Buffer.from("\x89PNG\r\n\x1a\nbroken", "latin1").toString("base64")] }, 400, "could not be read"],
	["nine images", { prompt: "x", images: Array.from({ length: 9 }, () => png(2, 2)) }, 400, "at most 8"],
	["four large images", { prompt: "x", images: Array.from({ length: 4 }, () => png(1000, 1000, true)) }, 400, "too large together"],
	["a body over 16 MiB", { prompt: "x".repeat(17 << 20) }, 413, "larger than 16 MiB"],
];
for (const [what, body, status, error] of refused) {
	const res = await call("POST", "/threads", body);
	if (res.status !== status || !String(res.json?.error).includes(error)) await fail(`${what}: ${res.status} ${JSON.stringify(res.json)}`);
}
if ((await call("GET", "/threads")).json.length !== threads) await fail("a refused message created a Thread");
log("6. bad images and oversized bodies get a plain error, and create no Thread", refused.map((r) => r[0]).join(", "));

if ((await call("DELETE", `/threads/${id}`)).status !== 204) await fail("delete");
await pass();
