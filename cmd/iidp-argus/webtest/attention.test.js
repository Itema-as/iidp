// The automatic camera's rules (#103, #106), run on a clock the tests
// move. Run with: node --test cmd/iidp-argus/webtest (Node 22.12 or later;
// not part of CI, which runs no Node).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createAttention, chipText, IDLE, LINGER, FLIGHT } from '../web/js/attention.js';

let id = 0;
/** @returns {import('../web/js/types.js').Note} */
const note = (application, loudness, extra = {}) => ({ id: ++id, at: '2026-09-28T10:00:00Z', place: application ? { application } : {}, loudness, message: '', ...extra });

/** Steps the brain from t to until, every 50 ms, collecting its commands. */
function run(a, from, until, exists) {
  const out = [];
  for (let t = from; t <= until; t += 50) {
    const c = a.step(t, exists);
    if (c) out.push({ t, ...c });
  }
  return out;
}

test('idle, it goes home once and circles there', () => {
  const a = createAttention();
  const cmds = run(a, 0, 5000);
  assert.deepEqual(cmds.map((c) => c.type), ['home']);
  assert.equal(a.mode, 'watching');
});

test('a note waits 0.7 s for a burst to finish landing, then is visited', () => {
  const a = createAttention();
  run(a, 0, 1000);
  a.note(note('shop', 'quiet'), 1000);
  assert.equal(a.step(1500), null);
  const c = a.step(1750);
  assert.deepEqual(c, { type: 'visit', key: 'shop', loudness: 'quiet' });
  assert.equal(a.mode, 'showing');
  assert.equal(a.showing, 'shop');
});

test('it lingers 9, 7 or 4 s by loudness, then goes back out', () => {
  for (const loudness of ['loud', 'normal', 'quiet']) {
    const a = createAttention();
    a.note(note('shop', loudness), 0);
    const cmds = run(a, 700, 20000);
    assert.equal(cmds[0].type, 'visit');
    const home = cmds.find((c) => c.type === 'home');
    assert.ok(home, `${loudness}: back out`);
    const lingered = home.t - cmds[0].t;
    assert.ok(lingered >= FLIGHT + LINGER[loudness] && lingered < FLIGHT + LINGER[loudness] + 100, `${loudness} lingered ${lingered}`);
  }
});

test('one entry per place: a newer or louder note replaces it', () => {
  const a = createAttention({ enabled: true });
  a.input(0); // hold, so that nothing is visited yet
  a.note(note('shop', 'quiet'), 100);
  a.note(note('shop', 'loud'), 200);
  a.note(note('shop', 'quiet'), 300); // quieter: keeps loud, refreshed
  a.note(note(undefined, 'normal', { place: { component: 'traefik' } }), 400);
  a.step(450);
  const waiting = a.waiting();
  assert.deepEqual(waiting.map((e) => [e.key, e.loudness]).sort(), [['platform', 'normal'], ['shop', 'loud']]);
});

test('loudest first, then oldest', () => {
  const a = createAttention();
  a.input(0);
  a.note(note('a', 'quiet'), 100);
  a.note(note('b', 'normal'), 200);
  a.note(note('c', 'normal'), 300);
  a.note(note('d', 'loud'), 400);
  const visits = run(a, IDLE + 1, IDLE + 60000).filter((c) => c.type === 'visit').map((c) => c.key);
  // a is quiet and has expired (20 s) by the time the others are done.
  assert.deepEqual(visits.slice(0, 3), ['d', 'b', 'c']);
});

test('a louder note interrupts a quieter visit', () => {
  const a = createAttention();
  a.note(note('shop', 'quiet'), 0);
  run(a, 0, 1000);
  a.note(note('api', 'loud'), 2000);
  const cmds = run(a, 2000, 3000);
  assert.deepEqual(cmds.map((c) => c.key), ['api']);
});

test('quiet entries expire after 20 s and normal after 60 s; loud wait until shown', () => {
  const a = createAttention();
  a.input(0);
  a.note(note('q', 'quiet'), 0);
  a.note(note('n', 'normal'), 0);
  a.note(note('l', 'loud'), 0);
  a.pin(true, 0); // a card holds the camera for a long time
  run(a, 0, 21000);
  a.pin(false, 0);
  assert.deepEqual(a.waiting().map((e) => e.key).sort(), ['l', 'n']);
  a.pin(true, 0);
  run(a, 21000, 61000);
  assert.deepEqual(a.waiting().map((e) => e.key), ['l']);
});

test('three or more places within 3.7 s give one wide shot, then only the loud ones are visited', () => {
  const a = createAttention();
  run(a, 0, 1000);
  a.note(note('a', 'quiet'), 1000);
  a.note(note('b', 'normal'), 1500);
  a.note(note('c', 'loud'), 2000);
  const cmds = run(a, 2000, 20000);
  assert.deepEqual(cmds[0], { t: 2700, type: 'wide', keys: ['a', 'b', 'c'] });
  assert.equal(chipText('wide', { showing: null, resumesIn: 0 }), 'Automatic camera: several changes at once');
  const visits = cmds.filter((c) => c.type === 'visit').map((c) => c.key);
  assert.deepEqual(visits, ['c']);
});

test('at most one wide shot every 8 s', () => {
  const a = createAttention();
  a.note(note('a', 'quiet'), 0);
  a.note(note('b', 'quiet'), 100);
  a.note(note('c', 'quiet'), 200);
  const first = run(a, 200, 1000).filter((c) => c.type === 'wide');
  a.note(note('d', 'quiet'), 7500);
  a.note(note('e', 'quiet'), 7600);
  a.note(note('f', 'quiet'), 7700);
  const second = run(a, 7700, 11000).filter((c) => c.type === 'wide');
  assert.equal(first.length, 1);
  assert.equal(second.length, 1);
  assert.ok(second[0].t - first[0].t > 8000, `the second came ${second[0].t - first[0].t} ms after the first`);
});

test('dragging, rotating or scrolling holds the camera still for 12 s', () => {
  const a = createAttention();
  run(a, 0, 1000);
  a.input(1000);
  assert.deepEqual(a.step(1050), { type: 'hold' });
  a.note(note('api', 'loud'), 2000);
  assert.equal(run(a, 1100, 1000 + IDLE - 50).length, 0);
  assert.equal(a.mode, 'looking');
  assert.equal(chipText(a.mode, { showing: null, resumesIn: a.resumesIn(5000) }), 'Automatic camera paused while you look around, back in 8 s');
  // Meanwhile, loud and normal entries are shown as edge markers.
  assert.deepEqual(a.waiting().map((e) => e.key), ['api']);
  const after = run(a, 1000 + IDLE, 1000 + IDLE + 200);
  assert.deepEqual(after.map((c) => c.key), ['api']);
  assert.deepEqual(a.waiting(), []);
});

test('a pinned card holds the camera, and closing it starts the usual pause', () => {
  const a = createAttention();
  run(a, 0, 1000);
  a.pin(true, 1000);
  assert.deepEqual(a.step(1050), { type: 'hold' });
  assert.equal(a.mode, 'pinned');
  a.note(note('api', 'normal'), 1100);
  assert.equal(run(a, 1100, 30000).length, 0);
  assert.deepEqual(a.waiting().map((e) => e.key), ['api']);
  a.pin(false, 30000);
  assert.equal(a.step(30050), null);
  assert.equal(a.mode, 'looking');
  assert.deepEqual(run(a, 30000 + IDLE, 30000 + IDLE + 100).map((c) => c.key), ['api']);
});

test('with the cluster lost it pulls back and visits nothing until it returns', () => {
  const a = createAttention();
  run(a, 0, 1000);
  a.setLost(true);
  a.note(note('api', 'loud'), 1000);
  const lost = run(a, 1000, 20000);
  assert.deepEqual(lost.map((c) => c.type), ['home']);
  assert.equal(a.mode, 'lost');
  assert.equal(chipText('lost', { showing: null, resumesIn: 0, lost: 'cluster' }), 'Automatic camera holding back: the cluster is lost');
  a.setLost(false);
  assert.deepEqual(run(a, 20000, 20100).map((c) => c.key), ['api']);
});

test('the chip turns the camera off and on; off, it never moves', () => {
  const a = createAttention();
  a.setEnabled(false);
  a.note(note('api', 'loud'), 0);
  assert.deepEqual(run(a, 0, 5000).map((c) => c.type), ['hold']);
  assert.equal(chipText(a.mode, { showing: null, resumesIn: 0 }), 'Automatic camera off');
  assert.deepEqual(a.waiting(), []);
  a.setEnabled(true);
  assert.deepEqual(run(a, 5000, 5100).map((c) => c.key), ['api']);
});

test('it can start off, for someone who prefers reduced motion', () => {
  const a = createAttention({ enabled: false });
  assert.equal(a.enabled, false);
  assert.equal(a.mode, 'off');
});

test('notes that only report something finished, seeded ones and the seam do not move it', () => {
  const a = createAttention();
  a.note(note('shop', 'quiet', { feedOnly: true }), 0);
  a.note(note('shop', 'loud', { seeded: true }), 0);
  a.note(note(undefined, 'quiet', { seam: true }), 0);
  assert.deepEqual(run(a, 0, 5000).map((c) => c.type), ['home']);
});

test('an entry for a place that is gone is dropped', () => {
  const a = createAttention();
  a.note(note('gone', 'loud'), 0);
  assert.deepEqual(run(a, 0, 3000, (k) => k !== 'gone').map((c) => c.type), ['home']);
});

test('a place flown to by hand has been seen', () => {
  const a = createAttention();
  a.input(0);
  a.note(note('api', 'loud'), 100);
  a.seen('api');
  assert.deepEqual(a.waiting(), []);
});
