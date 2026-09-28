// @ts-check
// The page's model: the domain objects from the stream, replaced whole as
// the stream sends them, plus what only the page knows: each
// Application's orbit slot, kept for life, and the times the page saw
// things happen, from which every animation runs, so that a redraw never
// restarts one (#115 "Animations run from timestamps held in the model").
// Times are the page's clock (performance.now()), except the notes' own.

import { addNote, trim } from './feed.js';
import { deployKey } from './deploys.js';

/** @typedef {import('./types.js').Application} Application */
/** @typedef {import('./types.js').Environment} Environment */
/** @typedef {import('./types.js').Component} Component */
/** @typedef {import('./types.js').Note} Note */
/** @typedef {import('./types.js').Snapshot} Snapshot */
/** @typedef {import('./types.js').ClusterState} ClusterState */
/** @typedef {import('./types.js').PlatformLinks} PlatformLinks */
/** @typedef {import('./types.js').Deploy} Deploy */
/** @typedef {import('./deploys.js').DeployLife} DeployLife */

/** How long a gone Environment takes to crumble, and a gone Application to sink, in ms. */
export const CRUMBLE = 1500;
export const SINK = 2400;
/** How long the stream may be down before the page shows the last known state as lost. */
export const STREAM_GRACE = 4000;

/**
 * What the page knows of an Environment beyond the stream: when it was
 * born (-Infinity when it was already there at the first snapshot), when
 * it started leaving, when it went (and crumbles), and whether it was an
 * Unreleased plot.
 * @typedef {{born: number, leaving: number, gone: number, wasUnreleased: boolean, last: Environment}} EnvLife
 */

/**
 * @typedef {{
 *   name: string, app: Application, slot: number, born: number, gone: number,
 *   ghosts: Environment[]
 * }} AppView
 */

export function createModel() {
  /** @type {Map<string, AppView>} */
  const apps = new Map();
  /** @type {Map<string, Component>} */
  const components = new Map();
  /** @type {Map<string, EnvLife>} */
  const envLives = new Map();
  /** @type {Map<string, DeployLife>} */
  const deployLives = new Map();
  /** @type {number[]} */
  let free = [];
  let high = 0;

  const model = {
    apps,
    components,
    /** @type {Note[]} */
    feed: /** @type {Note[]} */ ([]),
    /** @type {ClusterState} */
    cluster: /** @type {ClusterState} */ ({ state: 'connected' }),
    /** @type {PlatformLinks} */
    platform: /** @type {PlatformLinks} */ ({}),
    restartedAt: '',
    ready: false,
    snapshots: 0,
    /** Bumps whenever an Application, Environment or component comes or goes, for the drawing. */
    structure: 0,
    /** @type {{state: 'connecting' | 'open' | 'down', since: number}} */
    connection: { state: /** @type {'connecting' | 'open' | 'down'} */ ('connecting'), since: 0 },

    /**
     * A snapshot replaces everything. Applications keep their slots; the
     * first snapshot's are taken in name order, and the page saw none of
     * what it holds happen.
     * @param {Snapshot} s @param {number} t @param {number} nowMs
     */
    snapshot(s, t, nowMs) {
      const first = model.snapshots === 0;
      model.snapshots++;
      model.cluster = s.cluster;
      model.platform = s.platform ?? {};
      model.restartedAt = s.restartedAt;
      model.ready = s.ready;
      model.feed = trim(s.feed ?? [], nowMs);
      const names = new Set(s.applications.map((a) => a.name));
      for (const name of apps.keys()) if (!names.has(name)) model.applicationRemoved(name, t);
      for (const a of [...s.applications].sort((x, y) => (x.name < y.name ? -1 : 1))) update(a, t, !first);
      const comps = new Set(s.components.map((c) => c.name));
      for (const name of [...components.keys()]) if (!comps.has(name)) model.componentRemoved(name);
      for (const c of s.components) model.component(c);
      model.structure++;
    },

    /** @param {Application} a @param {number} t */
    application(a, t) {
      update(a, t, true);
    },

    /** @param {string} name @param {number} t */
    applicationRemoved(name, t) {
      const v = apps.get(name);
      if (!v || v.gone) return;
      v.gone = t;
      for (const e of v.app.environments) {
        const life = envLives.get(`${name}/${e.name}`);
        if (life && !life.gone) life.gone = t;
      }
      model.structure++;
    },

    /** @param {Component} c */
    component(c) {
      if (!components.has(c.name)) model.structure++;
      components.set(c.name, c);
    },

    /** @param {string} name */
    componentRemoved(name) {
      if (components.delete(name)) model.structure++;
    },

    /** @param {Note} n @param {number} nowMs */
    note(n, nowMs) {
      model.feed = addNote(model.feed, n, nowMs);
    },

    /** @param {ClusterState} c */
    setCluster(c) {
      model.cluster = c;
    },

    /** @param {'connecting' | 'open' | 'down'} state @param {number} t */
    setConnection(state, t) {
      if (state !== model.connection.state) model.connection = { state, since: t };
    },

    /**
     * Finishes what has gone: a crumbled Environment and a sunk
     * Application are dropped, and the slot is free for the next one.
     * @param {number} t
     */
    tick(t) {
      for (const [name, v] of apps) {
        if (v.gone && t - v.gone > SINK) {
          apps.delete(name);
          free.push(v.slot);
          for (const key of envLives.keys()) if (key.startsWith(`${name}/`)) envLives.delete(key);
          model.structure++;
          continue;
        }
        const before = v.ghosts.length;
        v.ghosts = v.ghosts.filter((e) => {
          const life = envLives.get(`${name}/${e.name}`);
          const keep = !!life && t - life.gone <= CRUMBLE;
          if (!keep) envLives.delete(`${name}/${e.name}`);
          return keep;
        });
        if (v.ghosts.length !== before) model.structure++;
      }
    },

    /**
     * Whether the page shows the last known state as lost, and why: Argus
     * has lost the cluster, or the page has lost its stream to Argus.
     * @param {number} t
     * @returns {'cluster' | 'stream' | null}
     */
    lost(t) {
      if (model.snapshots > 0 && model.connection.state !== 'open' && t - model.connection.since > STREAM_GRACE) return 'stream';
      if (model.cluster.state === 'lost') return 'cluster';
      return null;
    },

    /** @param {string} app @param {string} env */
    envLife(app, env) {
      return envLives.get(`${app}/${env}`);
    },

    /**
     * @param {string} app @param {string} env
     * @returns {(d: Deploy) => DeployLife | undefined}
     */
    deployLife(app, env) {
      return (d) => deployLives.get(`${app}/${env}/${deployKey(d)}`);
    },

    /**
     * Whether the whole Application is new: the page saw it arrive, and
     * every Environment it has is still arriving.
     * @param {string} name
     */
    isNewApp(name) {
      const v = apps.get(name);
      return !!v && v.born > -Infinity && v.app.environments.length > 0 && v.app.environments.every((e) => e.activity?.state === 'Arriving');
    },
  };

  function takeSlot() {
    if (free.length) {
      free.sort((a, b) => a - b);
      return /** @type {number} */ (free.shift());
    }
    return ++high;
  }

  /**
   * Replaces an Application whole, noting what the page saw change.
   * @param {Application} a @param {number} t @param {boolean} live
   */
  function update(a, t, live) {
    let v = apps.get(a.name);
    if (!v || v.gone) {
      if (v) free.push(v.slot);
      v = { name: a.name, app: a, slot: takeSlot(), born: live ? t : -Infinity, gone: 0, ghosts: [] };
      apps.set(a.name, v);
      model.structure++;
    }
    const was = new Set(v.app.environments.map((e) => e.name));
    const now = new Set(a.environments.map((e) => e.name));
    if (was.size !== now.size || [...now].some((n) => !was.has(n))) model.structure++;
    for (const e of v.app.environments) {
      if (now.has(e.name)) continue;
      const life = envLives.get(`${a.name}/${e.name}`);
      if (life && !life.gone) {
        life.gone = t;
        v.ghosts.push(e);
      }
    }
    for (const e of a.environments) {
      const key = `${a.name}/${e.name}`;
      let life = envLives.get(key);
      const state = e.activity?.state;
      if (!life || life.gone) {
        life = { born: live ? t : -Infinity, leaving: 0, gone: 0, wasUnreleased: false, last: e };
        envLives.set(key, life);
        v.ghosts = v.ghosts.filter((g) => g.name !== e.name);
      } else if (state === 'Arriving' && life.last.activity?.state !== 'Arriving') {
        // A first image into an Unreleased plot: it arrives again.
        life.born = t;
      }
      if (state === 'Leaving' && !life.leaving) life.leaving = live ? t : t - 60000;
      if (state !== 'Leaving') life.leaving = 0;
      if (state === 'Unreleased') life.wasUnreleased = true;
      life.last = e;

      const seen = new Set();
      for (const d of e.deploys ?? []) {
        const dk = `${key}/${deployKey(d)}`;
        seen.add(dk);
        const hop = d.hop ?? '';
        let dl = deployLives.get(dk);
        if (!dl) {
          dl = { first: t, live, hop, hopAt: live ? t : -Infinity, supersededAt: 0, lastHop: '' };
          deployLives.set(dk, dl);
          continue;
        }
        if (d.supersededBy && !dl.supersededAt) dl.supersededAt = t;
        if (hop !== dl.hop) {
          dl.lastHop = dl.hop;
          dl.hop = hop;
          dl.hopAt = t;
        }
      }
      for (const dk of deployLives.keys()) if (dk.startsWith(`${key}/`) && !seen.has(dk)) deployLives.delete(dk);
    }
    v.app = a;
  }

  return model;
}

/** @typedef {ReturnType<typeof createModel>} Model */
