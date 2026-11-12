import type { Api, Model, Models, ModelThinkingLevel, ThinkingLevel } from "@earendil-works/pi-ai";
import { clampThinkingLevel, getSupportedThinkingLevels } from "@earendil-works/pi-ai/models";
import type { ModelRef } from "@earendil-works/pi-durable";
import { DEFAULT_IMAGE_LIMITS, type ImageLimits } from "@earendil-works/pi-durable/tools";
import { compile, str } from "@earendil-works/pi-schema";

export type ModelChoice = { ref: string; name: string; thinking: ThinkingLevel[]; defaultThinking: ModelThinkingLevel };
export type ThreadModel = { model: ModelRef; thinkingLevel: ModelThinkingLevel };

const Text = compile(str());

const DEFAULT_THINKING: ThinkingLevel = "high";

// A model names another as it pleases: sonnet, Sonnet 5.5, claude-sonnet-5-5 or anthropic/claude-sonnet-5-5.
const key = (name: string) => name.toLowerCase().replace(/^.*\//, "").replace(/^claude-/, "").replace(/[^a-z0-9]+/g, "-");
const family = (name: string) => name.replace(/[\d.\s]+$/, "").toLowerCase();
const version = (name: string) => (name.match(/[\d.]+$/)?.[0] ?? "0").split(".").map(Number);
const newer = (a: number[], b: number[]) => {
	for (let i = 0; i < Math.max(a.length, b.length); i++) if ((a[i] ?? 0) !== (b[i] ?? 0)) return (a[i] ?? 0) > (b[i] ?? 0);
	return false;
};

// The models a Thread can run on: the default model and the newest of each family ("Opus 5.5" over "Opus 4.8") of the
// default model's provider, so far Claude through the owner's plan.
export class Catalog {
	readonly #models: Models;
	readonly #default: ModelRef;

	constructor(models: Models, defaultModel: ModelRef) {
		this.#models = models;
		this.#default = defaultModel;
	}

	// At start-up, before Threads resume, and after the owner signs in to another plan.
	async refresh(): Promise<void> {
		const { errors } = await this.#models.refresh({ providers: [this.#default.provider] });
		const error = errors.get(this.#default.provider);
		if (error) console.error(`models: ${error.message}`);
	}

	#offered() {
		const all = this.#models.getModels(this.#default.provider);
		const newest = new Map<string, (typeof all)[number]>();
		for (const m of all) {
			const seen = newest.get(family(m.name));
			if (!seen || newer(version(m.name), version(seen.name))) newest.set(family(m.name), m);
		}
		return all.filter((m) => m.id === this.#default.modelId || [...newest.values()].includes(m));
	}

	list(): ModelChoice[] {
		const all = this.#offered();
		const first = all.find((m) => m.id === this.#default.modelId);
		return [first, ...all.filter((m) => m !== first)].flatMap((m) =>
			m ? [{ ref: `${m.provider}/${m.id}`, name: m.name, thinking: getSupportedThinkingLevels(m).filter((l): l is ThinkingLevel => l !== "off"), defaultThinking: clampThinkingLevel(m, DEFAULT_THINKING) }] : [],
		);
	}

	// The offered models' ids, as a model reads them; all share the default model's provider.
	offered(): string {
		return this.#offered()
			.map((m) => `${m.id} (${m.name})`)
			.join(", ");
	}

	// A Thread's model, by an offered model's id or name, and thinking level, checked against what the model offers;
	// unset ones take the defaults.
	// oxlint-disable-next-line anti-slop/no-unknown-parameters -- this parses the request body's fields, which the route passes on unread
	choose(ref?: unknown, thinking?: unknown): ThreadModel {
		if (ref !== undefined && !Text.check(ref)) throw new RangeError("model must be a model id");
		const all = this.#offered();
		const found = ref ? (all.find((m) => key(m.id) === key(ref)) ?? all.find((m) => key(m.name) === key(ref))) : undefined;
		if (ref && !found) throw new RangeError(`not an offered model: ${ref}. Offered: ${this.offered()}`);
		const model = found ? { provider: found.provider, modelId: found.id } : this.#default;
		const known = this.#model(model);
		if (thinking !== undefined && !Text.check(thinking)) throw new RangeError("thinking level must be a string");
		return { model, thinkingLevel: this.#level(known, thinking) };
	}

	// Another model at the level nearest to the one given.
	near(ref: string, level: ModelThinkingLevel): ThreadModel {
		const { model } = this.choose(ref);
		return { model, thinkingLevel: clampThinkingLevel(this.#models.getModel(model.provider, model.modelId)!, level) };
	}

	// The rule Pi's `read` keeps for a conversation's model: the defaults, overridden by the model's own resize limits.
	imageLimits(model: ModelRef | undefined): ImageLimits {
		const resize = model && this.#models.getModel(model.provider, model.modelId)?.inputLimits?.images?.resize;
		return {
			maxWidth: resize?.maxWidth ?? DEFAULT_IMAGE_LIMITS.maxWidth,
			maxHeight: resize?.maxHeight ?? DEFAULT_IMAGE_LIMITS.maxHeight,
			maxBytes: resize?.maxBytes ?? DEFAULT_IMAGE_LIMITS.maxBytes,
		};
	}

	thinking(model: ModelRef, level?: string): ModelThinkingLevel {
		return this.#level(this.#model(model), level);
	}

	#model(model: ModelRef) {
		const found = this.#models.getModel(model.provider, model.modelId);
		if (!found) throw new RangeError(`unknown model ${model.provider}/${model.modelId}`);
		return found;
	}

	#level(found: Model<Api>, level?: string): ModelThinkingLevel {
		if (level === undefined) return clampThinkingLevel(found, DEFAULT_THINKING);
		const offered = getSupportedThinkingLevels(found);
		if (!offered.includes(level as ModelThinkingLevel)) throw new RangeError(`${found.name} does not offer thinking level ${level}`);
		return level as ModelThinkingLevel;
	}
}
