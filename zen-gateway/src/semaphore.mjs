// semaphore.mjs — per-model + global in-flight caps with FIFO queue.
export class Semaphore {
  #max;
  #inflight = 0;
  #queue = [];
  constructor(max) {
    this.#max = Math.max(1, max ?? 1);
  }
  get inflight() { return this.#inflight; }
  get queued() { return this.#queue.length; }
  async acquire(signal) {
    if (this.#inflight < this.#max) {
      this.#inflight += 1;
      return () => this.#release();
    }
    return new Promise((resolve, reject) => {
      const entry = { resolve, reject };
      this.#queue.push(entry);
      if (signal) {
        const onAbort = () => {
          const i = this.#queue.indexOf(entry);
          if (i >= 0) this.#queue.splice(i, 1);
          reject(Object.assign(new Error('aborted while queued'), { aborted: true }));
        };
        if (signal.aborted) onAbort();
        else signal.addEventListener('abort', onAbort, { once: true });
      }
    }).then(() => {
      this.#inflight += 1;
      return () => this.#release();
    });
  }
  #release() {
    this.#inflight = Math.max(0, this.#inflight - 1);
    const next = this.#queue.shift();
    if (next) next.resolve();
  }
  setMax(max) {
    this.#max = Math.max(1, max ?? 1);
    while (this.#inflight < this.#max && this.#queue.length > 0) {
      const next = this.#queue.shift();
      next.resolve();
    }
  }
}

export class Gate {
  #perModel = new Map();
  #global;
  #perModelMax; #globalMax;
  constructor(perModelMax, globalMax) {
    this.#perModelMax = perModelMax;
    this.#globalMax = globalMax;
    this.#global = new Semaphore(globalMax);
  }
  async acquire(model, signal) {
    const releaseGlobal = await this.#global.acquire(signal);
    let sem = this.#perModel.get(model);
    if (!sem) {
      sem = new Semaphore(this.#perModelMax);
      this.#perModel.set(model, sem);
      if (this.#perModel.size > 200) {
        const first = this.#perModel.keys().next().value;
        this.#perModel.delete(first);
      }
    }
    let releaseModel;
    try {
      releaseModel = await sem.acquire(signal);
    } catch (e) {
      releaseGlobal();
      throw e;
    }
    return () => { releaseModel(); releaseGlobal(); };
  }
  stats() {
    const per = {};
    for (const [k, s] of this.#perModel) per[k] = { inflight: s.inflight, queued: s.queued };
    return { global: { inflight: this.#global.inflight, queued: this.#global.queued }, perModel: per };
  }
}
