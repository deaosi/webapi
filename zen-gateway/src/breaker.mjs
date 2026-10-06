// breaker.mjs — per-model cooldown after 429 (60s base, exponential, 30m cap).
export class Breaker {
  #until = new Map();
  #streak = new Map();
  #baseMs; #maxMs;
  constructor(baseMs = 60000, maxMs = 30 * 60000) {
    this.#baseMs = baseMs;
    this.#maxMs = maxMs;
  }
  /** Milliseconds until the model is usable again (0 = usable now). */
  blockedMs(model, now = Date.now()) {
    const until = this.#until.get(model) ?? 0;
    return Math.max(0, until - now);
  }
  markLimited(model, now = Date.now()) {
    const streak = (this.#streak.get(model) ?? 0) + 1;
    this.#streak.set(model, streak);
    const ms = Math.min(this.#baseMs * 2 ** (streak - 1), this.#maxMs);
    this.#until.set(model, now + ms);
    return ms;
  }
  markOk(model) {
    this.#streak.delete(model);
    this.#until.delete(model);
  }
  snapshot(now = Date.now()) {
    const out = {};
    for (const [k, until] of this.#until) {
      if (until > now) out[k] = { cooldown_ms: until - now, streak: this.#streak.get(k) ?? 0 };
    }
    return out;
  }
}
