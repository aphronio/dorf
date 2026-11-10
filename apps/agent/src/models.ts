import type { Models, ModelThinkingLevel, ThinkingLevel } from "@earendil-works/pi-ai";
import { clampThinkingLevel, getSupportedThinkingLevels } from "@earendil-works/pi-ai/models";
import type { ModelRef } from "@earendil-works/pi-durable";

export type ModelChoice = { ref: string; name: string; thinking: ThinkingLevel[]; defaultThinking: ModelThinkingLevel };

const DEFAULT_THINKING: ThinkingLevel = "high";

// The models a Thread can run on: those of the default model's provider, so far Claude through the owner's plan.
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

	list(): ModelChoice[] {
		const all = this.#models.getModels(this.#default.provider);
		const first = all.find((m) => m.id === this.#default.modelId);
		return [first, ...all.filter((m) => m !== first)].flatMap((m) =>
			m ? [{ ref: `${m.provider}/${m.id}`, name: m.name, thinking: getSupportedThinkingLevels(m).filter((l): l is ThinkingLevel => l !== "off"), defaultThinking: clampThinkingLevel(m, DEFAULT_THINKING) }] : [],
		);
	}

	// A Thread's model and thinking level, checked against what the model offers; unset ones take the defaults.
	choose(ref?: unknown, thinking?: unknown): { model: ModelRef; thinkingLevel: ModelThinkingLevel } {
		if (ref !== undefined && typeof ref !== "string") throw new RangeError("model must be provider/model");
		const slash = ref?.indexOf("/") ?? -1;
		const model = ref ? { provider: ref.slice(0, slash), modelId: ref.slice(slash + 1) } : this.#default;
		const found = this.#models.getModel(model.provider, model.modelId);
		if (!found || model.provider !== this.#default.provider) throw new RangeError(`not an offered model: ${ref}`);
		return { model, thinkingLevel: this.thinking(model, thinking) };
	}

	// Another model at the level nearest to the one given.
	near(ref: string, level: ModelThinkingLevel): { model: ModelRef; thinkingLevel: ModelThinkingLevel } {
		const { model } = this.choose(ref);
		return { model, thinkingLevel: clampThinkingLevel(this.#models.getModel(model.provider, model.modelId)!, level) };
	}

	thinking(model: ModelRef, level?: unknown): ModelThinkingLevel {
		const found = this.#models.getModel(model.provider, model.modelId);
		if (!found) throw new RangeError(`unknown model ${model.provider}/${model.modelId}`);
		if (level === undefined) return clampThinkingLevel(found, DEFAULT_THINKING);
		if (typeof level !== "string") throw new RangeError("thinking level must be a string");
		const offered = getSupportedThinkingLevels(found);
		if (!offered.includes(level as ModelThinkingLevel)) throw new RangeError(`${found.name} does not offer thinking level ${level}`);
		return level as ModelThinkingLevel;
	}
}
