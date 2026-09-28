// Where things sit (#102), and labels that do not pile up (#115).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { appLayout, componentPlaces, distance, orbitRadius, ringOf } from '../web/js/layout.js';
import { declutter } from '../web/js/labels.js';

test('ring k holds 6k slots, every other ring offset half a step', () => {
  const rings = [];
  for (let slot = 1; slot <= 18; slot++) rings.push(ringOf(slot).k);
  assert.deepEqual(rings, [1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2]);
  assert.equal(ringOf(19).k, 3);
  const step = (2 * Math.PI) / 12;
  assert.ok(Math.abs(ringOf(8).angle - ringOf(7).angle - step) < 1e-9);
  assert.ok(Math.abs(ringOf(7).angle - (Math.PI / 12 + Math.PI / 4)) < 1e-9, 'ring 2 is offset half a step');
});

test('Applications on their slots never overlap, and a slot is where it always was', () => {
  const hubs = [];
  for (let slot = 1; slot <= 30; slot++) hubs.push(appLayout(slot, ['prod', 'staging']).centre);
  for (let i = 0; i < hubs.length; i++) for (let j = i + 1; j < hubs.length; j++) assert.ok(distance(hubs[i], hubs[j]) > 9, `slots ${i + 1} and ${j + 1}`);
  assert.deepEqual(appLayout(5, ['prod']).centre, appLayout(5, ['prod', 'staging', 'pr-1']).centre);
  assert.equal(Math.round(Math.hypot(hubs[0][0], hubs[0][2])), orbitRadius(1));
});

test('prod and staging, then three Preview Environments, and the rest counted', () => {
  const L = appLayout(1, ['prod', 'staging', 'pr-1', 'pr-2', 'pr-3', 'pr-4', 'pr-5']);
  assert.deepEqual(L.envs.map((e) => e.name), ['prod', 'staging', 'pr-1', 'pr-2', 'pr-3']);
  assert.equal(L.more, 2);
  assert.deepEqual(L.filament.end, L.centre, 'one filament, from the core to the hub');
});

test('components keep their places on the ring whichever others there are', () => {
  const a = componentPlaces(['argocd', 'traefik', 'deploy-gate']);
  const b = componentPlaces(['argocd', 'traefik', 'deploy-gate', 'cert-manager', 'something-new']);
  assert.deepEqual(a.get('argocd'), [0, 0, 0]);
  assert.deepEqual(a.get('traefik'), b.get('traefik'));
  assert.deepEqual(a.get('deploy-gate'), b.get('deploy-gate'));
  assert.ok(distance(b.get('something-new'), [0, 0, 0]) > distance(b.get('traefik'), [0, 0, 0]));
});

test('labels that would overlap: the more important one stays', () => {
  const shown = declutter([
    { id: 'env', x: 100, y: 100, w: 60, h: 14, priority: 40 },
    { id: 'tag', x: 110, y: 104, w: 70, h: 16, priority: 100 },
    { id: 'app', x: 300, y: 100, w: 60, h: 14, priority: 70 },
    { id: 'off', x: -200, y: 100, w: 60, h: 14, priority: 70 },
  ], { width: 800, height: 600 });
  assert.deepEqual([...shown].sort(), ['app', 'tag']);
});
