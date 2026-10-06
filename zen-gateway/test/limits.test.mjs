import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Semaphore, Gate } from '../src/semaphore.mjs';
import { Breaker } from '../src/breaker.mjs';

test('semaphore caps concurrency and queues the rest', async () => {
  const s = new Semaphore(2);
  const order = [];
  const r1 = await s.acquire();
  const r2 = await s.acquire();
  assert.equal(s.inflight, 2);
  let third = false;
  const p3 = s.acquire().then((rel) => { third = true; order.push('third'); return rel; });
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(third, false, 'third must wait');
  assert.equal(s.queued, 1);
  r1();
  const r3 = await p3;
  assert.equal(third, true);
  assert.equal(s.inflight, 2);
  r2(); r3();
  assert.equal(s.inflight, 0);
});

test('semaphore release hands the slot to the oldest waiter', async () => {
  const s = new Semaphore(1);
  const seen = [];
  const r1 = await s.acquire();
  const p2 = s.acquire().then((r) => { seen.push(2); return r; });
  const p3 = s.acquire().then((r) => { seen.push(3); return r; });
  r1();
  const r2 = await p2;
  assert.deepEqual(seen, [2], 'FIFO');
  r2();
  const r3 = await p3;
  assert.deepEqual(seen, [2, 3]);
  r3();
});

test('a queued acquire can be aborted without leaking a slot', async () => {
  const s = new Semaphore(1);
  const r1 = await s.acquire();
  const ctl = new AbortController();
  const p = s.acquire(ctl.signal);
  ctl.abort();
  await assert.rejects(p, (e) => e.aborted === true);
  assert.equal(s.queued, 0);
  assert.equal(s.inflight, 1);
  r1();
  assert.equal(s.inflight, 0);
});

test('gate enforces both the per-model and the global cap', async () => {
  const g = new Gate(1, 2);
  const a = await g.acquire('model-a');
  const b = await g.acquire('model-b');
  assert.equal(g.stats().global.inflight, 2);
  let third = false;
  const p = g.acquire('model-c').then((rel) => { third = true; return rel; });
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(third, false, 'global cap reached');
  let secondSameModel = false;
  const p2 = g.acquire('model-a').then((rel) => { secondSameModel = true; return rel; });
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(secondSameModel, false, 'per-model cap reached');
  a();
  const relC = await p;
  assert.equal(third, true);
  b(); relC();
  const relA2 = await p2;
  relA2();
  assert.equal(g.stats().global.inflight, 0);
});

test('breaker cools a model after a 429 and recovers on success', () => {
  const b = new Breaker(1000, 8000);
  let now = 100000;
  assert.equal(b.blockedMs('m', now), 0);
  assert.equal(b.markLimited('m', now), 1000);
  assert.equal(b.blockedMs('m', now), 1000);
  now += 500;
  assert.equal(b.blockedMs('m', now), 500);
  assert.equal(b.markLimited('m', now), 2000, 'exponential');
  now += 2000;
  assert.equal(b.blockedMs('m', now), 0, 'expired');
  b.markOk('m');
  assert.deepEqual(b.snapshot(now), {});
});

test('breaker backs off exponentially but stops at the cap', () => {
  const b = new Breaker(1000, 4000);
  let now = 0;
  const waits = [];
  for (let i = 0; i < 5; i += 1) waits.push(b.markLimited('m', now));
  assert.deepEqual(waits, [1000, 2000, 4000, 4000, 4000]);
  assert.equal(Object.keys(b.snapshot(now)).length, 1);
});
