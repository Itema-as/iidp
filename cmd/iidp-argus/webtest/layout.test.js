import { test } from 'node:test';
import assert from 'node:assert/strict';
import { appLayout, componentPlaces, distance, orbitRadius, ringOf, COMPONENT_GROUPS, COMPONENT_RADIUS, CORE_RADIUS, FLOOR } from '../web/js/layout.js';
import { COMPONENTS } from '../web/js/card.js';
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

test('components keep their places in their groups whichever others there are', () => {
  const a = componentPlaces(['argocd', 'traefik', 'deploy-gate']);
  const b = componentPlaces(['argocd', 'traefik', 'deploy-gate', 'cert-manager', 'k3s', 'something-new']);
  assert.deepEqual(a.places.get('argocd'), [0, 0, 0]);
  assert.deepEqual(a.places.get('traefik'), b.places.get('traefik'));
  assert.deepEqual(a.places.get('deploy-gate'), b.places.get('deploy-gate'));
  assert.deepEqual(b.places.get('k3s'), FLOOR);
  assert.ok(distance(b.places.get('something-new'), [0, 0, 0]) > distance(b.places.get('traefik'), [0, 0, 0]));
  assert.deepEqual(a.groups.map((g) => g.key), ['edge', 'delivery'], 'a group none of whose members the Platform has is not drawn');
  assert.deepEqual(b.groups.map((g) => [g.key, g.members]), [['edge', ['traefik', 'cert-manager']], ['delivery', ['deploy-gate']], ['other', ['something-new']]]);
});

test('every known component but the core and the floor is in exactly one group', () => {
  const grouped = COMPONENT_GROUPS.flatMap((g) => g.members);
  assert.equal(new Set(grouped).size, grouped.length);
  assert.deepEqual([...grouped, 'argocd', 'k3s'].sort(), Object.keys(COMPONENTS).sort());
});

test('no two components overlap, nor any the core, and k3s is beneath the core', () => {
  const { places } = componentPlaces(Object.keys(COMPONENTS));
  const ring = [...places].filter(([n]) => n !== 'argocd' && n !== 'k3s');
  for (let i = 0; i < ring.length; i++) {
    assert.ok(distance(ring[i][1], [0, 0, 0]) > CORE_RADIUS + 3, ring[i][0]);
    for (let j = i + 1; j < ring.length; j++) assert.ok(distance(ring[i][1], ring[j][1]) > COMPONENT_RADIUS * 3, `${ring[i][0]} and ${ring[j][0]}`);
  }
  assert.ok(places.get('k3s')[1] < -CORE_RADIUS - 2);
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
