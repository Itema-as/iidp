// @ts-check
// What the detail card shows (#113, panel B): for an Environment, a
// Platform component and the Platform core, the peek's three key facts,
// everything "All details" adds, and the read-only links. It never shows
// logs, environment variables, or anything that acts.

import { notesAbout, toneOf } from './feed.js';
import { currentDeploy, hopStrip } from './deploys.js';
import { stateChips } from './look.js';

/** @typedef {import('./types.js').Application} Application */
/** @typedef {import('./types.js').Environment} Environment */
/** @typedef {import('./types.js').Component} Component */
/** @typedef {import('./types.js').Capability} Capability */
/** @typedef {import('./types.js').Note} Note */
/** @typedef {import('./types.js').PlatformLinks} PlatformLinks */
/** @typedef {import('./types.js').Run} Run */

/**
 * @typedef {{
 *   kind: string, title: string,
 *   chips: {text: string, tone: string}[],
 *   why: string | null,
 *   hops: ReturnType<typeof hopStrip> | null,
 *   peek: [string, string][],
 *   sections: {title: string, rows: [string, string][]}[],
 *   capabilities: {key: string, name: string, state: string, tone: string, note: string, outlined: boolean}[],
 *   links: {text: string, href: string, note: string}[],
 *   recent: {tone: string, at: string, message: string}[]
 * }} Card
 */

/** How many feed entries a card lists. */
const RECENT = 5;

/**
 * "just now", "12 min ago", "3 h ago", "2 d ago".
 * @param {string | undefined} iso
 * @param {number} nowMs
 */
export function ago(iso, nowMs) {
  if (!iso) return '';
  const s = Math.max(0, (nowMs - Date.parse(iso)) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86400)} d ago`;
}

/** @param {Run | null} run @param {number} nowMs */
function runText(run, nowMs) {
  if (!run) return 'none has run';
  const when = run.finishedAt ?? run.startedAt;
  return when ? `${run.result} ${ago(when, nowMs)}` : run.result;
}

/** @param {Capability} c */
export function capabilityName(c) {
  switch (c.type) {
    case 'postgres':
      return `Postgres ${c.name}`;
    case 'itema-login':
      return 'Itema login';
    case 'custom-domain':
      return `Custom domain ${c.name}`;
    default:
      return `Scheduled task ${c.name}`;
  }
}

/** A Capability's key, for outlining it on its Environment's card. @param {Capability} c */
export function capabilityKey(c) {
  return `${c.type}:${c.name}`;
}

/** @param {Capability} c */
function capabilityState(c) {
  if (c.activity) return { state: c.activity.stuck ? `${c.activity.state}, stuck` : c.activity.state, tone: c.activity.stuck ? 'stuck' : 'working' };
  if (c.condition.state === 'Degraded') return { state: 'Degraded', tone: 'degraded' };
  if (c.condition.state === 'Unknown') return { state: 'Unknown', tone: 'unknown' };
  if (c.condition.warning) return { state: 'Warning', tone: 'warn' };
  return { state: 'Healthy', tone: 'healthy' };
}

/** @param {Note} n */
function recentEntry(n) {
  return { tone: toneOf(n), at: n.at, message: n.message };
}

/**
 * An Environment's card. capability, when given, is the key of the
 * Capability that was clicked, which the card outlines.
 * @param {string} app
 * @param {Environment} env
 * @param {{feed: Note[], platform: PlatformLinks, nowMs: number, capability?: string}} ctx
 * @returns {Card}
 */
export function environmentCard(app, env, { feed, platform, nowMs, capability }) {
  const preview = env.name !== 'prod' && env.name !== 'staging';
  const a = env.activity;
  const unreleased = a?.state === 'Unreleased';
  let why = null;
  if (a?.stuck && a.reason) why = `Stuck: ${a.reason}`;
  else if (env.condition?.state === 'Degraded' && env.condition.reason) why = `Degraded: ${env.condition.reason}`;
  else if (env.condition?.state === 'Unknown' && env.condition.reason) why = `Unknown: ${env.condition.reason}`;

  const image = env.image ? `${env.image.repository}:${env.image.tag}` : unreleased ? 'none yet: no Deploy has reached it' : 'none yet';
  const deployed = env.image?.deployedAt ? ago(env.image.deployedAt, nowMs) : env.image ? 'unknown: Argus did not see its Deploy' : '';
  const pods = env.pods.total === 0 && !env.image ? 'none' : `${env.pods.ready} of ${env.pods.total} ready, ${env.pods.restarts} restart${env.pods.restarts === 1 ? '' : 's'}`;
  const addresses = env.addresses.length ? env.addresses.join(', ') : 'none';

  /** @type {[string, string][]} */
  const running = [['Image', image]];
  if (deployed) running.push(['Deployed', deployed]);
  running.push(['Pods', pods], ['Addresses', addresses]);
  /** @type {{title: string, rows: [string, string][]}[]} */
  const sections = [{ title: 'Running', rows: running }];

  const postgres = env.capabilities.find((c) => c.type === 'postgres');
  if (env.migration || postgres) {
    /** @type {[string, string][]} */
    const rows = [['Last migration', runText(env.migration, nowMs)]];
    if (postgres) rows.push(['Backups', postgres.condition.warning ? `failing: ${postgres.condition.warning.replace(/^backups are failing: /, '')}` : 'no failure reported']);
    sections.push({ title: 'Database', rows });
  }
  if (env.tasks.length) {
    sections.push({ title: 'Scheduled tasks', rows: env.tasks.map((t) => /** @type {[string, string]} */ ([t.name, `${t.schedule}, last run ${runText(t.lastRun, nowMs)}`])) });
  }
  if (env.argocd) {
    const op = env.argocd.operation;
    sections.push({
      title: 'ArgoCD',
      rows: [
        ['Application', env.argocd.application],
        ['Sync', env.argocd.sync],
        ['Health', env.argocd.health],
        ['Last sync', op ? `${op.phase}${op.finishedAt ? ' ' + ago(op.finishedAt, nowMs) : ''}` : 'none'],
      ],
    });
  }

  /** @type {Card['links']} */
  const links = [];
  if (env.addresses[0] && !unreleased) links.push({ text: env.addresses[0].replace(/^https?:\/\//, ''), href: env.addresses[0], note: 'open it' });
  if (env.links?.argocd) links.push({ text: 'ArgoCD', href: env.links.argocd, note: 'sync and resources' });
  if (env.links?.grafana) links.push({ text: 'Grafana', href: env.links.grafana, note: 'logs' });
  const last = [...env.deploys].reverse().find((d) => !d.refused && d.commit);
  if (last?.commit && platform.platformRepository) {
    links.push({
      text: `${last.promote ? 'Promote' : 'Deploy'} ${last.tag}, commit ${last.commit.slice(0, 7)}`,
      href: `${platform.platformRepository}/commit/${last.commit}`,
      note: 'in the Platform repository',
    });
  }

  const d = currentDeploy(env);
  /** @type {[string, string][]} */
  const peek = [];
  if (why) peek.push(['Why', why.replace(/^\w+: /, '')]);
  peek.push(['Image', env.image ? env.image.tag : image]);
  if (!why && deployed) peek.push(['Deployed', deployed]);
  peek.push(['Pods', pods]);

  return {
    kind: preview ? 'Preview Environment' : 'Environment',
    title: `${app} ${env.name}`,
    chips: stateChips(env.condition, a),
    why,
    hops: d ? hopStrip(d) : null,
    peek: peek.slice(0, 3),
    sections,
    capabilities: env.capabilities.map((c) => ({
      key: capabilityKey(c), name: capabilityName(c), ...capabilityState(c),
      note: c.condition.warning ?? c.activity?.reason ?? c.condition.reason ?? '',
      outlined: capability === capabilityKey(c),
    })),
    links,
    recent: notesAbout(feed, (p) => p.application === app && p.environment === env.name, RECENT).map(recentEntry),
  };
}

/** What each Platform component is, in words, and what to call it. */
export const COMPONENTS = {
  argocd: ['ArgoCD', 'Keeps every Environment in step with the Platform repository'],
  argus: ['Argus', 'This view: watches the cluster and streams what it sees'],
  'cert-manager': ['cert-manager', 'Issues and renews the certificates'],
  'cloudnative-pg': ['CloudNativePG', 'Runs the Postgres databases'],
  'cnpg-barman-cloud': ['Barman Cloud', 'Backs the databases up to Object Storage'],
  'deploy-gate': ['Deploy gate', 'Checks and records every Deploy and Promote'],
  'external-dns': ['external-dns', "Keeps the DNS records for the Platform's addresses"],
  guardrails: ['Guardrails', 'Admission policies on the Application namespaces'],
  monitoring: ['Monitoring', 'Ships logs and metrics to Grafana Cloud'],
  'oauth2-proxy': ['Itema login', 'Signs people in before the Environments that ask for it'],
  'platform-tls': ['Platform TLS', 'The wildcard certificate for the base domain'],
  traefik: ['Traefik', 'Routes requests to the Environments and terminates TLS'],
  k3s: ['k3s', 'The node and Kubernetes itself'],
};

/** @param {string} name */
export function componentName(name) {
  return /** @type {Record<string, string[]>} */ (COMPONENTS)[name]?.[0] ?? name;
}

/**
 * A Platform component's card.
 * @param {Component} c
 * @param {{feed: Note[], platform: PlatformLinks}} ctx
 * @returns {Card}
 */
export function componentCard(c, { feed, platform }) {
  const what = /** @type {Record<string, string[]>} */ (COMPONENTS)[c.name]?.[1] ?? 'A Platform component';
  const managed = c.name !== 'traefik' && c.name !== 'k3s';
  let why = null;
  if (c.activity?.stuck && c.activity.reason) why = `Stuck: ${c.activity.reason}`;
  else if (c.condition.state !== 'Healthy' && c.condition.reason) why = `${c.condition.state}: ${c.condition.reason}`;
  /** @type {[string, string][]} */
  const rows = [['What it does', what], ['Version', c.version || 'not known to Argus']];
  if (!managed) rows.push(['Managed by', c.name === 'k3s' ? 'k3s itself, not ArgoCD' : 'k3s, not ArgoCD']);

  /** @type {Card['links']} */
  const links = [];
  if (managed && platform.argocdURL) links.push({ text: 'ArgoCD', href: `${platform.argocdURL}/applications/argocd/${encodeURIComponent(c.name)}`, note: 'sync and resources' });
  if (platform.grafanaURL) links.push({ text: 'Grafana', href: platform.grafanaURL, note: 'logs and metrics' });
  if (managed && c.name !== 'argocd' && platform.bootstrapRepository) {
    links.push({
      text: `bootstrap/templates/${c.name}.yaml`,
      href: `${platform.bootstrapRepository}/blob/${encodeURIComponent(platform.bootstrapRevision || 'HEAD')}/bootstrap/templates/${encodeURIComponent(c.name)}.yaml`,
      note: 'how it is installed',
    });
  }
  const peek = /** @type {[string, string][]} */ ([...(why ? [['Why', why.replace(/^\w+: /, '')]] : []), ['What it does', what], ['Version', c.version || 'not known']]);
  return {
    kind: 'Platform component',
    title: componentName(c.name),
    chips: stateChips(c.condition, c.activity),
    why,
    hops: null,
    peek: peek.slice(0, 3),
    sections: [{ title: 'Component', rows }],
    capabilities: [],
    links,
    recent: notesAbout(feed, (p) => p.component === c.name, RECENT).map(recentEntry),
  };
}

/**
 * The Platform core's card: a summary of the Platform, and ArgoCD's own
 * facts.
 * @param {Application[]} apps
 * @param {Component | undefined} argocd
 * @param {{feed: Note[], platform: PlatformLinks, components: Component[]}} ctx
 * @returns {Card}
 */
export function coreCard(apps, argocd, { feed, platform, components }) {
  const envs = apps.flatMap((a) => a.environments.map((e) => ({ app: a.name, e })));
  const attention = [
    ...envs.filter(({ e }) => e.activity?.stuck || e.condition?.state === 'Degraded').map(({ app, e }) => `${app} ${e.name}`),
    ...components.filter((c) => c.activity?.stuck || c.condition.state === 'Degraded').map((c) => componentName(c.name)),
  ];
  const deploying = envs.filter(({ e }) => e.activity?.state === 'Deploying' && !e.activity.stuck).map(({ app, e }) => `${app} ${e.name}`);
  /** @type {[string, string][]} */
  const now = [
    ['Applications', String(apps.length)],
    ['Environments', String(envs.length)],
    ['Needs attention', attention.length ? attention.join(', ') : 'nothing'],
    ['Deploying', deploying.length ? deploying.join(', ') : 'nothing'],
  ];
  /** @type {Card['links']} */
  const links = [];
  if (platform.argocdURL) links.push({ text: 'ArgoCD', href: platform.argocdURL, note: 'every Environment' });
  if (platform.grafanaURL) links.push({ text: 'Grafana', href: platform.grafanaURL, note: 'logs and metrics' });
  if (platform.platformRepository) links.push({ text: 'Platform repository', href: platform.platformRepository, note: 'desired state and its history' });
  return {
    kind: 'Platform',
    title: 'The Platform',
    chips: argocd ? stateChips(argocd.condition, argocd.activity) : [],
    why: null,
    hops: null,
    peek: now.slice(0, 3),
    sections: [
      { title: 'Now', rows: now },
      { title: 'ArgoCD', rows: [['What it does', COMPONENTS.argocd[1]], ['Version', argocd?.version || 'not known to Argus']] },
    ],
    capabilities: [],
    links,
    recent: notesAbout(feed, () => true, RECENT).map(recentEntry),
  };
}
