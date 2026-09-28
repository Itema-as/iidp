// @ts-check
// The automatic camera's rules (#103, with #106's loudness): what it
// should look at, and when it must hold still. This module decides; the
// drawing flies. Every function takes the time, so the rules can be run
// without a clock or a browser.

import { placeKey } from './feed.js';

/** @typedef {import('./types.js').Note} Note */
/** @typedef {import('./types.js').Loudness} Loudness */

/** How long the camera lingers on a place, by the loudness of what happened there. */
export const LINGER = { loud: 9000, normal: 7000, quiet: 4000 };
/** How long an entry waits unseen before it is dropped; loud ones wait until shown. */
export const EXPIRE = { loud: Infinity, normal: 60000, quiet: 20000 };
/** @type {Record<Loudness, number>} */
export const RANK = { quiet: 1, normal: 2, loud: 3 };
/** How long after the latest note the camera waits, so that a burst can finish landing. */
export const SETTLE = 700;
/** Three or more places within this long make a burst, shown in one wide shot. */
export const BURST_WINDOW = 3700;
export const BURST_PLACES = 3;
/** At most one wide shot this often. */
export const WIDE_EVERY = 8000;
/** How long the wide shot lasts before the loud entries are visited. */
export const WIDE_LASTS = 7500;
/** The camera resumes this long after the last drag, rotation or scroll, or after a card closes. */
export const IDLE = 12000;
/** A visit starts with the flight there, which the linger does not count. */
export const FLIGHT = 1500;

/**
 * @typedef {{key: string, loudness: Loudness, rank: number, at: number}} Entry
 * @typedef {'watching' | 'showing' | 'wide' | 'looking' | 'pinned' | 'lost' | 'off'} Mode
 * @typedef {{type: 'home'} | {type: 'visit', key: string, loudness: Loudness} | {type: 'wide', keys: string[]} | {type: 'hold'}} Command
 */

/**
 * The camera's brain. note() hears the stream's notes; input(), pin(),
 * setEnabled() and setLost() hear the person and the connection; step()
 * says, once a frame, what the camera should do now, or null for "carry
 * on".
 * @param {{enabled?: boolean}} [options] enabled is whether the automatic
 *   camera starts on: it does, unless the person prefers reduced motion.
 */
export function createAttention({ enabled = true } = {}) {
  /** @type {Map<string, Entry>} */
  const queue = new Map();
  /** @type {{key: string, at: number}[]} */
  let recent = [];
  let lastNote = -Infinity;
  let lastInput = -Infinity;
  let lastWide = -Infinity;
  let until = 0;
  let pinned = false;
  let lost = false;
  let on = enabled;
  /** @type {Mode} */
  let mode = on ? 'watching' : 'off';
  /** @type {Entry | null} */
  let current = null;
  let home = false;
  let held = false;

  return {
    /**
     * A note from the stream. Only notes that ask the camera to look count:
     * not those that only report something finished, the seeded ones, or
     * the seam. One entry per Application or the Platform; a newer or
     * louder note replaces the older one, and a quieter one keeps the
     * louder entry but refreshes it.
     * @param {Note} n
     * @param {number} t
     */
    note(n, t) {
      if (n.feedOnly || n.seeded || n.seam) return;
      const key = placeKey(n.place);
      const rank = RANK[n.loudness] ?? 1;
      const old = queue.get(key);
      if (!old || rank >= old.rank) queue.set(key, { key, loudness: n.loudness, rank, at: t });
      else old.at = t;
      lastNote = t;
      recent.push({ key, at: t });
    },

    /** Someone dragged, rotated or scrolled the map. @param {number} t */
    input(t) {
      lastInput = t;
    },

    /**
     * A card was pinned or closed. Closing starts the usual pause.
     * @param {boolean} open @param {number} t
     */
    pin(open, t) {
      if (pinned && !open) lastInput = t;
      pinned = open;
    },

    /** The chip turned the automatic camera on or off. @param {boolean} value */
    setEnabled(value) {
      on = value;
      if (on) lastInput = -Infinity;
    },

    get enabled() {
      return on;
    },

    /** Whether Argus has lost the cluster, or its stream. @param {boolean} value */
    setLost(value) {
      lost = value;
    },

    /** A place was flown to by hand (the feed, a marker): it has been seen. @param {string} key */
    seen(key) {
      queue.delete(key);
    },

    /**
     * What the camera should do now, or null to carry on.
     * @param {number} t
     * @param {(key: string) => boolean} [exists] whether a place is still
     *   on the map; an entry for one that is gone is dropped.
     * @returns {Command | null}
     */
    step(t, exists = () => true) {
      for (const [k, e] of queue) if (t - e.at > EXPIRE[e.loudness] || !exists(k)) queue.delete(k);
      recent = recent.filter((r) => t - r.at < BURST_WINDOW);

      /** @param {Mode} m */
      const holdAs = (m) => {
        mode = m;
        home = false;
        if (held) return null;
        held = true;
        return /** @type {Command} */ ({ type: 'hold' });
      };
      if (!on) return holdAs('off');
      if (pinned) return holdAs('pinned');
      if (t - lastInput < IDLE) return holdAs('looking');
      held = false;
      if (lost) {
        if (mode === 'lost') return null;
        mode = 'lost';
        home = true;
        return { type: 'home' };
      }
      if (mode === 'off' || mode === 'pinned' || mode === 'looking' || mode === 'lost') {
        mode = 'watching';
        home = false;
      }
      if (t - lastNote < SETTLE) return null;

      const places = [...new Set(recent.map((r) => r.key))].filter((k) => exists(k));
      if (places.length >= BURST_PLACES && t - lastWide > WIDE_EVERY && mode !== 'wide') {
        // One wide shot for the burst, then only its loud entries are visited.
        for (const k of places) {
          const e = queue.get(k);
          if (e && e.loudness !== 'loud') queue.delete(k);
        }
        mode = 'wide';
        home = false;
        lastWide = t;
        until = t + WIDE_LASTS;
        current = null;
        return { type: 'wide', keys: places };
      }

      const next = [...queue.values()].sort((a, b) => b.rank - a.rank || a.at - b.at)[0];
      if (mode === 'showing' && next && current && next.rank > current.rank) until = 0; // a louder note interrupts
      if ((mode === 'showing' || mode === 'wide') && t < until) return null;
      if (next) {
        queue.delete(next.key);
        mode = 'showing';
        home = false;
        current = next;
        until = t + FLIGHT + LINGER[next.loudness];
        return { type: 'visit', key: next.key, loudness: next.loudness };
      }
      current = null;
      mode = 'watching';
      if (home) return null;
      home = true;
      return { type: 'home' };
    },

    /** @returns {Mode} */
    get mode() {
      return mode;
    },

    /** The place being shown, while mode is showing. */
    get showing() {
      return mode === 'showing' && current ? current.key : null;
    },

    /**
     * Seconds until the camera resumes after someone looked around.
     * @param {number} t
     */
    resumesIn(t) {
      return Math.max(0, Math.ceil((IDLE - (t - lastInput)) / 1000));
    },

    /**
     * The loud and normal entries waiting, for the edge markers while the
     * camera holds still for someone.
     * @returns {Entry[]}
     */
    waiting() {
      if (!on || (mode !== 'looking' && mode !== 'pinned')) return [];
      return [...queue.values()].filter((e) => e.rank >= RANK.normal);
    },
  };
}

/**
 * What the chip says the camera is doing.
 * @param {Mode} mode
 * @param {{showing: string | null, resumesIn: number, lost?: 'cluster' | 'stream' | null}} detail
 */
export function chipText(mode, { showing, resumesIn, lost = 'cluster' }) {
  switch (mode) {
    case 'off':
      return 'Automatic camera off';
    case 'pinned':
      return 'Automatic camera paused while a card is open';
    case 'looking':
      return `Automatic camera paused while you look around, back in ${resumesIn} s`;
    case 'lost':
      return lost === 'stream' ? 'Automatic camera holding back: Argus cannot be reached' : 'Automatic camera holding back: the cluster is lost';
    case 'showing':
      return `Automatic camera showing ${showing === 'platform' ? 'the Platform' : showing}`;
    case 'wide':
      return 'Automatic camera: several changes at once';
    default:
      return 'Automatic camera watching';
  }
}
