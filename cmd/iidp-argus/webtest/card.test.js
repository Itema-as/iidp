import { test } from 'node:test';
import assert from 'node:assert/strict';
import { componentCard, coreCard, environmentCard } from '../web/js/card.js';
import { componentLook, envLook, TAGS, warningTag } from '../web/js/look.js';

const now = Date.parse('2026-09-28T12:00:00Z');
const platform = { argocdURL: 'https://argocd.example.test', grafanaURL: 'https://itema.grafana.net', platformRepository: 'https://github.com/Itema-as/iidp-platform', bootstrapRepository: 'https://github.com/Itema-as/iidp', bootstrapRevision: 'v1.2.3' };
const prod = {
  name: 'prod', namespace: 'shop-prod',
  argocd: { application: 'shop-prod', sync: 'Synced', health: 'Healthy', operation: { phase: 'Succeeded', finishedAt: '2026-09-28T11:00:00Z' } },
  image: { repository: 'ghcr.io/itema-as/shop', tag: '1.0.1', deployedAt: '2026-09-28T11:50:00Z' },
  pods: { ready: 1, total: 1, restarts: 2 },
  migration: { result: 'succeeded', finishedAt: '2026-09-28T11:49:00Z' },
  tasks: [{ name: 'report', schedule: '0 3 * * *', lastRun: { result: 'failed', finishedAt: '2026-09-28T03:01:00Z' } }],
  addresses: ['https://shop.app.itma.no'],
  links: { argocd: 'https://argocd.example.test/applications/argocd/shop-prod', grafana: 'https://itema.grafana.net/explore?x' },
  condition: { state: 'Healthy' },
  activity: { state: 'Deploying', stuck: false, deploy: { tag: '1.0.2', at: 'x', hop: 'Applying' } },
  capabilities: [
    { type: 'postgres', name: 'shop-db', condition: { state: 'Healthy', warning: 'backups are failing: exit 2' }, activity: null },
    { type: 'itema-login', name: 'itema-login', condition: { state: 'Healthy' }, activity: null },
  ],
  deploys: [{ tag: '1.0.1', commit: 'abcdef1234567890', at: 'y', hop: 'Serving' }, { tag: '1.0.2', commit: '1234567abcdef', at: 'x', hop: 'Applying' }],
};
const feed = [
  { id: 1, at: '2026-09-28T11:59:00Z', place: { application: 'shop', environment: 'prod' }, loudness: 'quiet', message: 'Deploy 1.0.2 to shop prod accepted' },
  { id: 2, at: '2026-09-28T11:59:30Z', place: { application: 'api', environment: 'prod' }, loudness: 'loud', message: 'api prod is Degraded' },
];

test("an Environment's card: state, hops, facts, Capabilities, links and its feed", () => {
  const card = environmentCard('shop', prod, { feed, platform, nowMs: now, capability: 'postgres:shop-db' });
  assert.equal(card.title, 'shop prod');
  assert.deepEqual(card.chips.map((c) => c.text), ['Healthy', 'Deploying']);
  assert.equal(card.hops.caption, 'Deploy of 1.0.2 is being applied');
  assert.deepEqual(card.peek, [['Image', '1.0.1'], ['Deployed', '10 min ago'], ['Pods', '1 of 1 ready, 2 restarts']]);
  assert.deepEqual(card.sections.map((s) => s.title), ['Running', 'Database', 'Scheduled tasks', 'ArgoCD']);
  assert.deepEqual(card.sections[1].rows, [['Last migration', 'succeeded 11 min ago'], ['Backups', 'failing: exit 2']]);
  assert.deepEqual(card.capabilities.map((c) => [c.name, c.state, c.outlined]), [['Postgres shop-db', 'Warning', true], ['Itema login', 'Healthy', false]]);
  assert.deepEqual(card.links.map((l) => l.href), [
    'https://shop.app.itma.no',
    'https://argocd.example.test/applications/argocd/shop-prod',
    'https://itema.grafana.net/explore?x',
    'https://github.com/Itema-as/iidp-platform/commit/1234567abcdef',
  ]);
  assert.deepEqual(card.recent.map((r) => r.message), ['Deploy 1.0.2 to shop prod accepted']);
});

test('a stuck or Degraded Environment says why first', () => {
  const stuck = environmentCard('shop', { ...prod, activity: { state: 'Deploying', stuck: true, reason: 'the migration failed', deploy: { tag: '1.0.2', hop: 'Applying', stuck: true, reason: 'the migration failed' } } }, { feed, platform, nowMs: now });
  assert.equal(stuck.why, 'Stuck: the migration failed');
  assert.deepEqual(stuck.peek[0], ['Why', 'the migration failed']);
  assert.equal(stuck.hops.caption, 'Deploy of 1.0.2 is stuck at Applying: the migration failed');
});

test("a Platform component's card links to ArgoCD, Grafana and its place in bootstrap/; Traefik's not to ArgoCD", () => {
  const gate = componentCard({ name: 'deploy-gate', version: '1.4.0', condition: { state: 'Healthy' }, activity: null }, { feed, platform });
  assert.equal(gate.title, 'Deploy gate');
  assert.deepEqual(gate.links.map((l) => l.text), ['ArgoCD', 'Grafana', 'bootstrap/templates/deploy-gate.yaml']);
  assert.equal(gate.links[2].href, 'https://github.com/Itema-as/iidp/blob/v1.2.3/bootstrap/templates/deploy-gate.yaml');
  const traefik = componentCard({ name: 'traefik', condition: { state: 'Degraded', reason: 'traefik-0 is in CrashLoopBackOff' }, activity: null }, { feed, platform });
  assert.deepEqual(traefik.links.map((l) => l.text), ['Grafana']);
  assert.equal(traefik.why, 'Degraded: traefik-0 is in CrashLoopBackOff');
});

test("the core's card sums up the Platform", () => {
  const apps = [{ name: 'shop', environments: [prod] }, { name: 'api', environments: [{ ...prod, name: 'prod', condition: { state: 'Degraded' }, activity: null }] }];
  const card = coreCard(apps, { name: 'argocd', version: 'v3.1.8', condition: { state: 'Healthy' }, activity: null }, { feed, platform, components: [] });
  assert.deepEqual(card.sections[0].rows, [['Applications', '2'], ['Environments', '2'], ['Needs attention', 'api prod'], ['Deploying', 'shop prod']]);
  assert.deepEqual(card.links.map((l) => l.text), ['ArgoCD', 'Grafana', 'Platform repository']);
});

test('loud looks have a shape and a marked tag, not only a colour', () => {
  const base = { ...prod, activity: null };
  const stuck = envLook({ ...base, activity: { state: 'Deploying', stuck: true } });
  const degraded = envLook({ ...base, condition: { state: 'Degraded' } });
  const unknown = envLook({ ...base, condition: { state: 'Unknown' } });
  const unreleased = envLook({ ...base, activity: { state: 'Unreleased', stuck: false } });
  assert.deepEqual([stuck.shape, degraded.shape, unknown.shape, unreleased.shape], ['ring', 'broken', 'dotted', 'plot']);
  assert.equal(new Set([stuck.shape, degraded.shape, unknown.shape, unreleased.shape]).size, 4);
  for (const tag of [TAGS.stuck, TAGS.degraded, TAGS.unknown]) assert.match(tag.text, /^[■✕?] /);
  // Stuck wins over Degraded, and Degraded over Arriving and Unknown.
  assert.equal(envLook({ ...base, condition: { state: 'Degraded' }, activity: { state: 'Deploying', stuck: true } }).tone, 'stuck');
  assert.equal(envLook({ ...base, condition: { state: 'Degraded' }, activity: { state: 'Arriving', stuck: false } }).tone, 'degraded');
  assert.equal(envLook({ ...base, activity: { state: 'Updating', stuck: false } }).wave, true);
  assert.equal(envLook({ ...base, activity: { state: 'Arriving', stuck: false } }, { preview: true }).tag, null, 'a preview arrives quietly');
  assert.equal(componentLook({ name: 'traefik', condition: { state: 'Degraded' }, activity: null }).shape, 'broken');
});

test("a Scheduled task's Warning tag says whether its last run failed or cannot start", () => {
  const task = (warning) => ({ type: 'scheduled-task', name: 'heartbeat', condition: { state: 'Healthy', warning }, activity: null });
  assert.equal(warningTag(task('the last run failed at 2026-09-29 10:00 UTC')), '! TASK FAILED');
  assert.equal(warningTag(task("the last run's Pod hello-heartbeat-1-x is Unschedulable: 0/1 nodes are available: 1 Insufficient cpu.")), '! TASK PENDING');
  assert.equal(warningTag(task(undefined)), null);
});
