// The page's model: slots kept for life, and the times animations run from.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createModel, CRUMBLE, SINK, STREAM_GRACE } from '../web/js/model.js';

const env = (name, extra = {}) => ({ name, pods: { ready: 1, total: 1, restarts: 0 }, tasks: [], addresses: [], capabilities: [], deploys: [], activity: null, condition: { state: 'Healthy' }, ...extra });
const app = (name, envs = [env('prod')]) => ({ name, environments: envs });
const snapshot = (apps) => ({ at: '', restartedAt: '', ready: true, cluster: { state: 'connected' }, applications: apps, components: [], feed: [] });

test('slots are taken in name order, kept for life, and a freed slot is reused', () => {
  const m = createModel();
  m.snapshot(snapshot([app('shop'), app('api'), app('hello')]), 0, Date.now());
  assert.deepEqual(['api', 'hello', 'shop'].map((n) => m.apps.get(n).slot), [1, 2, 3]);
  m.application(app('wiki'), 100);
  assert.equal(m.apps.get('wiki').slot, 4);
  assert.equal(m.apps.get('wiki').born, 100, 'the page saw it arrive');
  assert.equal(m.apps.get('api').born, -Infinity, 'it was there at the first snapshot');

  m.applicationRemoved('hello', 1000);
  assert.ok(m.apps.has('hello'), 'it sinks before it goes');
  m.tick(1000 + SINK + 1);
  assert.ok(!m.apps.has('hello'));
  m.application(app('ledger'), 5000);
  assert.equal(m.apps.get('ledger').slot, 2, 'the freed slot');
  assert.equal(m.apps.get('shop').slot, 3, 'nothing already drawn moves');
});

test('a reconnect snapshot keeps the slots of the Applications still there', () => {
  const m = createModel();
  m.snapshot(snapshot([app('api'), app('shop')]), 0, Date.now());
  m.snapshot(snapshot([app('shop'), app('zed')]), 1000, Date.now());
  assert.equal(m.apps.get('shop').slot, 2);
  assert.ok(m.apps.get('api').gone > 0, 'api sinks');
  assert.equal(m.apps.get('zed').slot, 3);
});

test('an Environment that goes crumbles, then is dropped', () => {
  const m = createModel();
  m.snapshot(snapshot([app('shop', [env('prod'), env('pr-1')])]), 0, Date.now());
  m.application(app('shop', [env('prod')]), 500);
  const v = m.apps.get('shop');
  assert.deepEqual(v.ghosts.map((e) => e.name), ['pr-1']);
  assert.equal(m.envLife('shop', 'pr-1').gone, 500);
  m.tick(500 + CRUMBLE + 1);
  assert.deepEqual(m.apps.get('shop').ghosts, []);
});

test('the page records when it saw an Environment arrive, leave, and a Deploy change hop', () => {
  const m = createModel();
  m.snapshot(snapshot([app('shop', [env('prod', { activity: { state: 'Unreleased', stuck: false } })])]), 0, Date.now());
  m.application(app('shop', [env('prod', { activity: { state: 'Arriving', stuck: false } })]), 200);
  assert.equal(m.envLife('shop', 'prod').born, 200, 'a first image arrives again');
  const deploy = (hop) => ({ tag: '1.0.1', at: 'x', hop });
  m.application(app('shop', [env('prod', { deploys: [deploy('Accepted')] })]), 300);
  m.application(app('shop', [env('prod', { deploys: [deploy('Applying')] })]), 400);
  const life = m.deployLife('shop', 'prod')(deploy('Applying'));
  assert.deepEqual([life.live, life.hop, life.hopAt, life.lastHop], [true, 'Applying', 400, 'Accepted']);
  m.application(app('shop', [env('prod', { activity: { state: 'Leaving', stuck: false } })]), 900);
  assert.equal(m.envLife('shop', 'prod').leaving, 900);
});

test('a whole Application is new while every Environment the page saw it arrive with arrives', () => {
  const m = createModel();
  m.snapshot(snapshot([]), 0, Date.now());
  m.application(app('ledger', [env('prod', { activity: { state: 'Arriving', stuck: false } })]), 100);
  assert.ok(m.isNewApp('ledger'));
  m.application(app('ledger', [env('prod')]), 200);
  assert.ok(!m.isNewApp('ledger'));
});

test('the picture is lost when the cluster is, or when the stream has been down a while', () => {
  const m = createModel();
  m.snapshot(snapshot([]), 0, Date.now());
  m.setConnection('open', 0);
  assert.equal(m.lost(10), null);
  m.setConnection('down', 1000);
  assert.equal(m.lost(1000 + STREAM_GRACE - 1), null);
  assert.equal(m.lost(1000 + STREAM_GRACE + 1), 'stream');
  m.setConnection('open', 9000);
  m.setCluster({ state: 'lost', since: 'x' });
  assert.equal(m.lost(9001), 'cluster');
});
