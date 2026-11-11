import type { ImageContent } from "@earendil-works/pi-ai";
import type { ImageLimits, ImageProcessor } from "@earendil-works/pi-durable/tools";

export const MAX_IMAGES = 8;
// The model's request carries the whole context, so one message's images stay well below a provider's request limit.
const MAX_TOTAL = 12 * 1024 * 1024;

const startsWith = (bytes: Uint8Array, at: number, ascii: string) => [...ascii].every((c, i) => bytes[at + i] === c.charCodeAt(0));

// The type the bytes are, whatever the browser called them: a stored entry is sent again with every request, and a
// provider refuses an image whose type does not match its data.
function typeOf(bytes: Uint8Array): string | undefined {
	if (startsWith(bytes, 0, "\x89PNG\r\n\x1a\n")) return "image/png";
	if (bytes[0] === 0xff && bytes[1] === 0xd8 && bytes[2] === 0xff) return "image/jpeg";
	if (startsWith(bytes, 0, "GIF87a") || startsWith(bytes, 0, "GIF89a")) return "image/gif";
	if (startsWith(bytes, 0, "RIFF") && startsWith(bytes, 8, "WEBP")) return "image/webp";
	if (startsWith(bytes, 0, "BM")) return "image/bmp";
}

// The size an image says it is, read from its header as Pi's `read` does (which does not export it); BMP added.
function dimensions(bytes: Uint8Array, type: string): { width: number; height: number } | undefined {
	const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
	switch (type) {
		case "image/png":
			return bytes.length < 24 ? undefined : { width: view.getUint32(16), height: view.getUint32(20) };
		case "image/gif":
			return bytes.length < 10 ? undefined : { width: view.getUint16(6, true), height: view.getUint16(8, true) };
		case "image/bmp":
			return bytes.length < 26 ? undefined : { width: Math.abs(view.getInt32(18, true)), height: Math.abs(view.getInt32(22, true)) };
		case "image/webp":
			if (bytes.length < 30) return undefined;
			if (startsWith(bytes, 12, "VP8 ")) return { width: view.getUint16(26, true) & 0x3fff, height: view.getUint16(28, true) & 0x3fff };
			if (startsWith(bytes, 12, "VP8L")) {
				const bits = view.getUint32(21, true);
				return { width: (bits & 0x3fff) + 1, height: ((bits >>> 14) & 0x3fff) + 1 };
			}
			if (startsWith(bytes, 12, "VP8X")) {
				const uint24 = (o: number) => view.getUint16(o, true) + (bytes[o + 2]! << 16);
				return { width: uint24(24) + 1, height: uint24(27) + 1 };
			}
			return undefined;
		case "image/jpeg":
			for (let o = 2; o + 9 <= bytes.length; ) {
				if (bytes[o] !== 0xff) return undefined;
				const marker = bytes[o + 1]!;
				if (marker === 0xff) {
					o++;
					continue;
				}
				// SOF0 to SOF15, except DHT (C4), JPG (C8) and DAC (CC), which share the range.
				if (marker >= 0xc0 && marker <= 0xcf && marker !== 0xc4 && marker !== 0xc8 && marker !== 0xcc)
					return { width: view.getUint16(o + 7), height: view.getUint16(o + 5) };
				o += 2 + view.getUint16(o + 2);
			}
			return undefined;
	}
}

// Decoding takes about 4 bytes a pixel, and a small file can claim a huge image: a 1 MB PNG of 20000 x 20000 holds the
// agent for seconds and takes over a GB. A 48-megapixel phone photo fits.
const MAX_PIXELS = 64_000_000;

// The processor, refusing an image whose header is unreadable or names more pixels than the bound, before it decodes
// anything. Both an attached image and one a Thread's `read` opens go through it.
export function bounded(processor: ImageProcessor): ImageProcessor {
	return {
		prepare(bytes, mimeType, limits) {
			const size = dimensions(bytes, typeOf(bytes) ?? mimeType);
			if (!size || size.width * size.height > MAX_PIXELS) return Promise.resolve(undefined);
			return processor.prepare(bytes, mimeType, limits);
		},
	};
}

// Images the owner attached, as Pi's `read` prepares one: upright, in a type every provider takes, within the
// Thread's model limits. Throws a RangeError, which the page shows, naming the image by its place in the message.
export async function prepareImages(processor: ImageProcessor, limits: ImageLimits, uploads: readonly string[]): Promise<ImageContent[]> {
	if (uploads.length > MAX_IMAGES) throw new RangeError(`at most ${MAX_IMAGES} images per message`);
	const images: ImageContent[] = [];
	let total = 0;
	for (const [i, upload] of uploads.entries()) {
		const name = uploads.length > 1 ? `image ${i + 1}` : "the image";
		let bytes: Uint8Array;
		try {
			bytes = Uint8Array.fromBase64(upload);
		} catch {
			throw new RangeError(`${name} is not base64`);
		}
		const type = typeOf(bytes);
		if (!type) throw new RangeError(`${name} is not a PNG, JPEG, GIF, WebP or BMP image`);
		const prepared = await processor.prepare(bytes, type, limits);
		if (!prepared) throw new RangeError(`${name} could not be read, or is larger than ${MAX_PIXELS / 1_000_000} megapixels`);
		total += prepared.data.length;
		if (total > MAX_TOTAL) throw new RangeError(`the images are too large together; send fewer at once`);
		images.push({ type: "image", data: prepared.data, mimeType: prepared.mimeType });
	}
	return images;
}
