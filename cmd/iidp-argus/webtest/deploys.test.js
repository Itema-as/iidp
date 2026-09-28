// Which hop a Deploy is on, and where its bead is (#106).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { beadsFor, hopStrip, TRAVEL } from '../web/js/deploys.js';

const env = (deploys, extra = {}) => ({ name: 'prod', deploys, capabilities: [], activity: null, ...extra });
const live = (hop, hopAt = 0, extra = {}) => ({ first: 0, live: true, hop, hopAt, supersededAt: 0, lastHop: '', ...extra });

test('the hop strip: done, now, stuck, still to come; a preview skips the gate', () => {
  const at = hopStrip({ tag: '1.0.1', hop: 'Applying' });
  assert.deepEqual(at.steps.map((s) => s.state), ['done', 'done', 'now', 'todo', 'todo']);
  assert.equal(at.caption, 'Deploy of 1.0.1 is being applied');
  const stuck = hopStrip({ tag: '1.0.1', hop: 'RollingOut', stuck: true, reason: 'the image cannot be pulled' });
  assert.deepEqual(stuck.steps.map((s) => s.state), ['done', 'done', 'done', 'stuck', 'todo']);
  assert.equal(stuck.caption, 'Deploy of 1.0.1 is stuck at Rolling out: the image cannot be pulled');
  assert.equal(hopStrip({ tag: '2.0.0', promote: true, hop: 'WaitingForArgoCD' }).caption, 'Promote of 2.0.0 is waiting for ArgoCD');
  assert.deepEqual(hopStrip({ tag: 'pr-1', preview: true, hop: 'Applying' }).steps.map((s) => s.state), ['skipped', 'skipped', 'now', 'todo', 'todo']);
  assert.deepEqual(hopStrip({ tag: '1', hop: 'Serving' }).steps.map((s) => s.state), ['done', 'done', 'done', 'done', 'done']);
  assert.equal(hopStrip({ tag: '1.0.1', supersededBy: '1.0.2' }).caption, 'Deploy of 1.0.1 superseded by 1.0.2');
});

test('each hop draws its bead on its own leg, and holds it at the end', () => {
  const cases = [
    ['Accepted', ['gate', 'coreEdge'], TRAVEL.gate, false],
    ['WaitingForArgoCD', ['coreEdge'], 0, true],
    ['Applying', ['coreEdge', 'hub'], TRAVEL.filament, false],
    ['RollingOut', ['hub', 'env'], TRAVEL.enter, false],
  ];
  for (const [hop, route, travel, pulse] of cases) {
    const d = { tag: '1.0.1', at: 't', hop };
    const half = beadsFor(env([d]), () => live(hop, 1000), 1000 + travel / 2);
    assert.equal(half.length, 1, hop);
    assert.deepEqual(half[0].route, route, hop);
    assert.equal(half[0].tone, 'deploy');
    if (travel) assert.ok(Math.abs(half[0].k - 0.5) < 0.01, `${hop}: halfway, k ${half[0].k}`);
    const held = beadsFor(env([d]), () => live(hop, 1000), 1000 + travel + 5000)[0];
    assert.equal(held.k, 1, `${hop} holds at the end of its leg`);
    assert.equal(held.pulse, pulse || hop === 'Applying', `${hop} pulse`);
  }
});

test('a Promote lifts off the staging cloud into the core', () => {
  const [b] = beadsFor(env([{ tag: '2.0.0', at: 't', hop: 'Accepted', promote: true }]), () => live('Accepted', 0), 100);
  assert.deepEqual(b.route, ['staging', 'hub', 'coreEdge']);
  assert.equal(b.tone, 'promote');
});

test('a stuck Deploy freezes where it stopped, blinking', () => {
  const [applying] = beadsFor(env([{ tag: '1', at: 't', hop: 'Applying', stuck: true, reason: 'the migration failed' }]), () => live('Applying', 0), 100);
  assert.deepEqual([applying.route, applying.k, applying.tone, applying.blink], [['coreEdge', 'hub'], 1, 'stuck', true]);
  const [rolling] = beadsFor(env([{ tag: '1', at: 't', hop: 'RollingOut', stuck: true }]), () => live('RollingOut', 0), 100);
  assert.ok(rolling.k > 0 && rolling.k < 1, 'part-way into the cloud');
});

test('Serving flashes once, only when the page saw it arrive', () => {
  const d = { tag: '1', at: 't', hop: 'Serving' };
  const [flash] = beadsFor(env([d]), () => live('Serving', 1000, { lastHop: 'RollingOut' }), 1100);
  assert.ok(flash.flash > 0.9);
  assert.equal(beadsFor(env([d]), () => live('Serving', 1000, { lastHop: 'RollingOut' }), 1000 + TRAVEL.flash + 1).length, 0);
  assert.equal(beadsFor(env([d]), () => ({ ...live('Serving', -Infinity), live: false }), 1100).length, 0, 'found serving in a snapshot');
});

test('a refused Deploy goes from the gate to the core and fizzles, if the page saw it', () => {
  const d = { tag: '9', at: 't', refused: true, reason: 'no such image' };
  const [b] = beadsFor(env([d]), () => live('', 0, { first: 1000 }), 1400);
  assert.deepEqual([b.route, b.tone], [['gate', 'core'], 'refused']);
  const late = beadsFor(env([d]), () => live('', 0, { first: 1000 }), 1000 + TRAVEL.fizzle * 0.9)[0];
  assert.ok(late.alpha < 0.5, 'fizzling out');
  assert.equal(beadsFor(env([d]), () => live('', 0, { first: 1000 }), 1000 + TRAVEL.fizzle + 1).length, 0);
  assert.equal(beadsFor(env([d]), () => ({ ...live('', 0), live: false }), 1400).length, 0, 'found refused in a snapshot');
});

test('a superseded Deploy dims and merges into the next one', () => {
  const d = { tag: '1', at: 't1', supersededBy: '2' };
  const [b] = beadsFor(env([d]), () => live('', 0, { supersededAt: 1000, lastHop: 'Applying' }), 1300);
  assert.deepEqual(b.route, ['hub', 'coreEdge']);
  assert.ok(b.alpha < 0.35);
  assert.equal(beadsFor(env([d]), () => live('', 0, { supersededAt: 1000, lastHop: 'Applying' }), 1000 + TRAVEL.merge + 1).length, 0);
});

test('a leaving database sends its final backup home along the filament', () => {
  const e = env([], { activity: { state: 'Leaving', stuck: false }, capabilities: [{ type: 'postgres', name: 'db', condition: { state: 'Healthy' }, activity: null }] });
  const beads = beadsFor(e, () => undefined, 500);
  assert.equal(beads.length, 3);
  assert.ok(beads.every((b) => b.tone === 'backup' && b.route.join() === 'hub,coreEdge'));
});
