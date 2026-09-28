// The stream's resilience: reloading, boundedly, when the Itema login
// session has expired.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { connect, loginExpired, reloadAllowed, retryDelay, FAILING_FOR, RELOADS_KEY } from '../web/js/stream.js';

const min = 60 * 1000;

test('reloads are bounded: spaced out, doubling, at most five in half an hour', () => {
  const now = 100 * min;
  assert.ok(reloadAllowed([], now));
  assert.ok(!reloadAllowed([now - 30 * 1000], now), 'not within a minute of the last');
  assert.ok(reloadAllowed([now - 61 * 1000], now));
  assert.ok(!reloadAllowed([now - 10 * min, now - 90 * 1000], now), 'two recent: two minutes apart');
  assert.ok(reloadAllowed([now - 10 * min, now - 121 * 1000], now));
  assert.ok(!reloadAllowed([1, 2, 3, 4, 5].map((i) => now - 29 * min + i), now), 'five in half an hour');
  assert.ok(reloadAllowed([now - 40 * min], now), 'old reloads are forgotten');
});

test('retrying by hand backs off to a minute', () => {
  assert.deepEqual([0, 1, 2, 5, 9].map(retryDelay), [3000, 6000, 12000, 60000, 60000]);
});

test("oauth2-proxy's redirect, or its 401, means the login session expired", () => {
  assert.ok(loginExpired({ type: 'opaqueredirect', status: 0 }));
  assert.ok(loginExpired({ type: 'basic', status: 401 }));
  assert.ok(!loginExpired({ type: 'basic', status: 200 }));
  assert.ok(!loginExpired({ type: 'basic', status: 503 }));
});

/** A stand-in EventSource the test drives. */
function fakeEventSource() {
  const made = [];
  class ES {
    static CLOSED = 2;
    constructor(url) {
      this.url = url;
      this.readyState = 0;
      this.listeners = {};
      made.push(this);
    }
    addEventListener(type, fn) {
      this.listeners[type] = fn;
    }
    close() {
      this.readyState = 2;
    }
    emit(type, data) {
      this.listeners[type]({ data: JSON.stringify(data) });
    }
  }
  return { ES, made };
}

const settle = () => new Promise((r) => setTimeout(r, 0));

function harness({ answer, history = [] }) {
  const { ES, made } = fakeEventSource();
  const store = new Map([[RELOADS_KEY, JSON.stringify(history)]]);
  const got = { messages: [], states: [], reloads: 0 };
  let clock = 1000 * min;
  const stream = connect({
    url: 'events',
    EventSource: /** @type {any} */ (ES),
    fetch: /** @type {any} */ (async () => answer()),
    storage: { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => store.set(k, v) },
    reload: () => got.reloads++,
    now: () => clock,
    log: () => {},
    onMessage: (type, data) => got.messages.push([type, data]),
    onState: (s) => got.states.push(s),
  });
  return { made, got, store, stream, advance: (ms) => (clock += ms) };
}

test('messages are parsed and handed on', () => {
  const h = harness({ answer: () => ({ type: 'basic', status: 200, ok: true }) });
  h.made[0].onopen();
  h.made[0].emit('snapshot', { applications: [] });
  h.made[0].emit('note', { id: 1 });
  assert.deepEqual(h.got.messages.map(([t]) => t), ['snapshot', 'note']);
  assert.deepEqual(h.got.states, ['connecting', 'open']);
  h.stream.close();
});

test('when EventSource gives up on the sign-in redirect, the page reloads, once', async () => {
  const h = harness({ answer: () => ({ type: 'opaqueredirect', status: 0, ok: false }) });
  h.made[0].readyState = 2;
  h.made[0].onerror();
  await settle();
  assert.equal(h.got.reloads, 1);
  assert.equal(JSON.parse(h.store.get(RELOADS_KEY)).length, 1);
  // The reloaded page fails the same way at once: it does not reload again.
  const again = harness({ answer: () => ({ type: 'opaqueredirect', status: 0, ok: false }), history: JSON.parse(h.store.get(RELOADS_KEY)) });
  again.made[0].readyState = 2;
  again.made[0].onerror();
  await settle();
  assert.equal(again.got.reloads, 0);
  h.stream.close();
  again.stream.close();
});

test('while Argus does not answer at all, the page keeps its picture and retries', async () => {
  const h = harness({ answer: () => Promise.reject(new TypeError('network')) });
  h.made[0].readyState = 2;
  h.made[0].onerror();
  await settle();
  assert.equal(h.got.reloads, 0);
  assert.equal(h.got.states.at(-1), 'down');
  h.stream.close();
});

test('a stream that keeps failing while Argus answers reloads the page after a while', async () => {
  const h = harness({ answer: () => ({ type: 'basic', status: 200, ok: true }) });
  h.made[0].onerror(); // EventSource retries by itself
  await settle();
  assert.equal(h.got.reloads, 0);
  h.advance(FAILING_FOR + 1);
  h.made[0].onerror();
  await settle();
  assert.equal(h.got.reloads, 1);
  h.stream.close();
});
