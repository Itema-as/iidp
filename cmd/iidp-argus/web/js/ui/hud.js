// @ts-check

import { h } from './dom.js';
import { mapFeed, notesSince, toneOf } from '../feed.js';

/** @typedef {import('../types.js').Note} Note */

/**
 * Which dot a folded feed's switch shows: the loudest among the new notes.
 * @type {Record<string, number>}
 */
const TONE_RANK = { loud: 4, normal: 3, quiet: 2, resolved: 1 };

/**
 * The chip: the camera's mode in words, and the switch.
 * @param {HTMLButtonElement} el
 * @param {() => void} onToggle
 */
export function createChip(el, onToggle) {
  el.addEventListener('click', onToggle);
  let text = '';
  let cls = '';
  return {
    /** @param {string} mode @param {string} words */
    update(mode, words) {
      const c = mode === 'off' ? 'off' : mode === 'looking' || mode === 'pinned' ? 'paused' : mode === 'lost' ? 'lost' : 'on';
      if (c !== cls) {
        el.dataset.state = c;
        el.setAttribute('aria-pressed', String(mode !== 'off'));
        cls = c;
      }
      if (words !== text) {
        /** @type {HTMLElement} */ (el.querySelector('.words')).textContent = words;
        el.title = mode === 'off' ? 'Turn the automatic camera on (Esc also resets the view)' : 'Turn the automatic camera off (Esc resets the view)';
        text = words;
      }
    },
  };
}

/**
 * The feed on the map: the latest six notes, newest at the bottom, each
 * with its loudness dot, message and age. A click flies to its place; the
 * pointer on one rings its place. The switch under it folds it away; while
 * folded, the switch counts the notes that came since, with the loudest
 * one's dot.
 * @param {HTMLElement} el
 * @param {HTMLButtonElement} toggle
 * @param {{onGo: (key: string) => void, onRing: (key: string | null) => void, folded: boolean, onFold: (folded: boolean) => void}} options
 */
export function createFeed(el, toggle, { onGo, onRing, folded, onFold }) {
  let last = '';
  let lastToggle = '';
  /** @type {Note[]} */
  let notes = [];
  /**
   * The newest note's time when the feed was folded, or, for a feed that
   * starts folded, the time the page loaded.
   * @type {string | null}
   */
  let since = new Date().toISOString();
  const fold = (/** @type {boolean} */ value) => {
    folded = value;
    if (folded) since = notes.at(-1)?.at ?? since;
    el.hidden = folded;
    toggle.setAttribute('aria-expanded', String(!folded));
  };
  const label = () => {
    const unseen = folded ? notesSince(notes, since) : [];
    const loudest = unseen.map(toneOf).sort((a, b) => (TONE_RANK[b] ?? 0) - (TONE_RANK[a] ?? 0))[0];
    const key = `${folded} ${unseen.length} ${loudest}`;
    if (key === lastToggle) return;
    lastToggle = key;
    toggle.title = folded ? 'Show recent changes' : 'Hide recent changes';
    toggle.replaceChildren(
      h('span', { class: 'caret', 'aria-hidden': 'true' }),
      h('span', {}, folded ? 'Recent changes' : 'Hide recent changes'),
      ...(unseen.length > 0 ? [h('span', { class: `dot ${loudest}`, 'aria-hidden': 'true' }), h('span', { class: 'new' }, `${unseen.length} new`)] : []),
    );
  };
  toggle.addEventListener('click', () => {
    fold(!folded);
    onFold(folded);
    label();
  });
  fold(folded);
  label();
  return {
    /** @param {Note[]} feed @param {number} nowMs */
    update(feed, nowMs) {
      notes = feed;
      label();
      if (folded) return;
      const entries = mapFeed(feed, nowMs);
      const key = JSON.stringify(entries);
      if (key === last) return;
      last = key;
      el.replaceChildren(...entries.map((e) => {
        const go = e.key;
        const li = h('li', { class: go ? e.tone : `${e.tone} nowhere` },
          h('span', { class: `dot ${e.tone}`, 'aria-hidden': 'true' }),
          h('span', { class: 'msg' }, e.message),
          h('span', { class: 'age' }, e.age));
        if (go) {
          const button = h('button', { type: 'button', class: 'go', 'aria-label': `Show ${go === 'platform' ? 'the Platform' : go}` });
          li.prepend(button);
          li.addEventListener('click', () => onGo(go));
          li.addEventListener('mouseenter', () => onRing(go));
          li.addEventListener('mouseleave', () => onRing(null));
        }
        return li;
      }));
    },
  };
}

/**
 * Edge markers: an arrow in the loudness's colour and the Application's
 * name, at the screen's edge towards a loud or normal change off screen.
 * A click flies there.
 * @param {HTMLElement} el
 * @param {(key: string) => void} onGo
 */
export function createEdges(el, onGo) {
  let last = '';
  return {
    /**
     * @param {{key: string, loudness: string, ndcX: number, ndcY: number, front: boolean}[]} items
     * @param {number} width @param {number} height
     */
    update(items, width, height) {
      const out = [];
      for (const i of items) {
        let x = i.ndcX;
        let y = i.ndcY;
        // A place behind the camera projects mirrored: flip it, and push it
        // off screen so it gets a marker.
        if (!i.front) {
          x = -x * 10;
          y = -y * 10;
        }
        if (Math.abs(x) < 0.95 && Math.abs(y) < 0.95) continue;
        const k = 1 / Math.max(Math.abs(x) / 0.86, Math.abs(y) / 0.82);
        const ex = x * k;
        const ey = Math.max(y * k, -0.6);
        out.push({ key: i.key, loudness: i.loudness, left: Math.round(((ex + 1) / 2) * width), top: Math.round(((1 - ey) / 2) * height), rot: Math.atan2(-y, x) });
      }
      const key = JSON.stringify(out.map((o) => [o.key, o.loudness, o.left >> 3, o.top >> 3, Math.round(o.rot * 8)]));
      if (key === last) return;
      last = key;
      el.replaceChildren(...out.map((o) => {
        const arrow = h('span', { class: 'arrow', 'aria-hidden': 'true' });
        arrow.style.transform = `rotate(${o.rot}rad)`;
        const b = h('button', { type: 'button', class: `edge ${o.loudness}`, onclick: () => onGo(o.key) }, arrow, h('span', {}, o.key === 'platform' ? 'the Platform' : o.key));
        b.style.transform = `translate(${o.left}px, ${o.top}px) translate(-50%, -50%)`;
        return b;
      }));
    },
  };
}

/**
 * The banner over a picture that is not live.
 * @param {HTMLElement} el
 */
export function createBanner(el) {
  let last = '';
  return {
    /** @param {string} text empty hides it */
    update(text) {
      if (text === last) return;
      last = text;
      el.textContent = text;
      el.hidden = !text;
    },
  };
}

/**
 * A frame-rate meter, for ?fps=1: frames a second over the last second,
 * the slowest frame in it, and what was drawn.
 * @param {HTMLElement} el
 */
export function createMeter(el) {
  el.hidden = false;
  let frames = 0;
  let worst = 0;
  let since = performance.now();
  let prev = since;
  return {
    /**
     * @param {number} t
     * @param {() => string} detail
     * @returns {number | null} the frame rate, once a second
     */
    tick(t, detail) {
      frames++;
      worst = Math.max(worst, t - prev);
      prev = t;
      if (t - since < 1000) return null;
      const fps = (frames * 1000) / (t - since);
      el.textContent = `${fps.toFixed(0)} fps · slowest frame ${worst.toFixed(0)} ms · ${detail()}`;
      frames = 0;
      worst = 0;
      since = t;
      return fps;
    },
  };
}
