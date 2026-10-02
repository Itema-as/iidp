// @ts-check
// The stream feeds the model; the model, the automatic camera's rules and the
// pointer feed the drawing, the card and what sits on the map.

import { createModel } from './model.js';
import { connect } from './stream.js';
import { createAttention, chipText } from './attention.js';
import { componentCard, coreCard, environmentCard } from './card.js';
import { createCardView } from './ui/card.js';
import { createBanner, createChip, createEdges, createFeed, createMeter } from './ui/hud.js';

/** @typedef {import('./types.js').Target} Target */

const $ = (/** @type {string} */ id) => /** @type {HTMLElement} */ (document.getElementById(id));
const params = new URLSearchParams(location.search);
const reduced = matchMedia('(prefers-reduced-motion: reduce)').matches;
// ?glow= scales the glow round the particles: 0 turns it off, 0.5 halves
// it, 2 doubles it.
const glowParam = parseFloat(params.get('glow') ?? '');
const glow = Number.isFinite(glowParam) ? Math.min(Math.max(glowParam, 0), 4) : 1;

const model = createModel();
const attention = createAttention({ enabled: !reduced });

/** @type {import('./draw/drawing.js').Drawing} */
let drawing;
try {
  const { createDrawing } = await import('./draw/drawing.js');
  drawing = createDrawing($('stage'), { reduced, glow, onInput: (t) => attention.input(t) });
} catch (err) {
  console.error('Argus could not start its drawing', err);
  $('fallback').hidden = false;
  throw err;
}

/** @type {Target | null} */
let hover = null;
/** @type {Target | null} */
let pinned = null;
/** @type {string | null} */
let ring = null;
let started = false;

const banner = createBanner($('banner'));
const chip = createChip(/** @type {HTMLButtonElement} */ ($('chip')), () => {
  attention.setEnabled(!attention.enabled);
  if (!attention.enabled) drawing.camera.hold();
});
/** Flying somewhere by hand counts as looking around. @param {string} key */
const go = (key) => {
  attention.input(performance.now());
  attention.seen(key);
  drawing.camera.go(key);
};
// The feed starts folded on a small screen; folding or unfolding it is
// remembered.
const FOLD = 'argus.feed';
const stored = (() => {
  try {
    return localStorage.getItem(FOLD);
  } catch {
    return null;
  }
})();
const feed = createFeed($('feed'), /** @type {HTMLButtonElement} */ ($('feed-toggle')), {
  onGo: go,
  onRing: (key) => (ring = key),
  folded: stored ? stored === 'folded' : matchMedia('(max-width: 700px), (max-height: 560px)').matches,
  onFold(folded) {
    if (folded) ring = null;
    try {
      localStorage.setItem(FOLD, folded ? 'folded' : 'open');
    } catch {
      // Not remembered, then.
    }
  },
});
const edges = createEdges($('edges'), go);
const meter = params.get('fps') === '1' ? createMeter($('meter')) : null;
const cardView = createCardView($('card'), { onClose: () => close() });

function close() {
  if (!pinned) return;
  pinned = null;
  attention.pin(false, performance.now());
  cardView.reset();
}

/** @param {Target} target */
function pin(target) {
  pinned = target;
  hover = null;
  cardView.reset();
  attention.pin(true, performance.now());
  drawing.camera.show(target);
}

connect({
  url: 'events',
  onState: (state) => model.setConnection(state, performance.now()),
  onMessage(type, data) {
    const t = performance.now();
    const nowMs = Date.now();
    switch (type) {
      case 'snapshot':
        model.snapshot(data, t, nowMs);
        if (!started) {
          started = true;
          drawing.camera.start(model);
        }
        break;
      case 'application':
        model.application(data, t);
        break;
      case 'application-removed':
        model.applicationRemoved(data.name, t);
        break;
      case 'component':
        model.component(data);
        break;
      case 'component-removed':
        model.componentRemoved(data.name);
        break;
      case 'note':
        model.note(data, nowMs);
        attention.note(data, t);
        break;
      case 'cluster':
        model.setCluster(data);
        break;
    }
  },
});

// The pointer: hovering peeks, a click pins, a click on empty space
// closes.
const stageEl = $('stage');
/** @type {{x: number, y: number} | null} */
let pressed = null;
stageEl.addEventListener('pointermove', (e) => {
  if (e.buttons) {
    hover = null;
    return;
  }
  const r = stageEl.getBoundingClientRect();
  hover = drawing.pick(e.clientX - r.left, e.clientY - r.top);
  stageEl.classList.toggle('pointing', !!hover);
});
stageEl.addEventListener('pointerleave', () => {
  hover = null;
});
stageEl.addEventListener('pointerdown', (e) => {
  pressed = e.button === 0 ? { x: e.clientX, y: e.clientY } : null;
});
stageEl.addEventListener('pointerup', (e) => {
  if (!pressed || Math.hypot(e.clientX - pressed.x, e.clientY - pressed.y) > 5) return;
  pressed = null;
  const r = stageEl.getBoundingClientRect();
  const target = drawing.pick(e.clientX - r.left, e.clientY - r.top);
  if (target) pin(target);
  else close();
});
// Esc closes the card and hands the view back to the automatic camera,
// turning it on if it was off; it starts again from home.
addEventListener('keydown', (e) => {
  if (e.key !== 'Escape') return;
  close();
  attention.resume();
});

/**
 * The card for a target, or null when it is gone from the model.
 * @param {Target} target @param {number} nowMs
 */
function cardFor(target, nowMs) {
  const ctx = { feed: model.feed, platform: model.platform, nowMs };
  if (target.kind === 'core') return coreCard([...model.apps.values()].filter((v) => !v.gone).map((v) => v.app), model.components.get('argocd'), { ...ctx, components: [...model.components.values()] });
  if (target.kind === 'component') {
    const c = model.components.get(target.name);
    return c ? componentCard(c, ctx) : null;
  }
  const v = model.apps.get(target.app);
  const env = v && !v.gone ? v.app.environments.find((e) => e.name === target.env) : undefined;
  // A Capability opens its Environment's card, with the Capability outlined.
  return env ? environmentCard(target.app, env, { ...ctx, capability: target.cap }) : null;
}

const exists = (/** @type {string} */ key) => key === 'platform' || (model.apps.has(key) && !model.apps.get(key)?.gone);
/** Whether what a card is about is still on the map. @param {Target} target */
const stillThere = (target) => target.kind === 'core'
  || (target.kind === 'component' ? model.components.has(target.name)
    : exists(target.app) && !!model.apps.get(target.app)?.app.environments.some((e) => e.name === target.env));
let lastSlow = 0;
let slowSeconds = 0;
let lastEdges = 0;
let lastFeed = 0;
let lastCard = 0;
let frames = 0;
let since = performance.now();

function frame() {
  requestAnimationFrame(frame);
  const t = performance.now();
  const nowMs = Date.now();
  model.tick(t);
  const lost = model.lost(t);
  attention.setLost(!!lost);
  if (started) {
    const cmd = attention.step(t, exists);
    if (cmd?.type === 'home') drawing.camera.home(lost ? 0.1 : 0.22);
    else if (cmd?.type === 'visit') drawing.camera.visit(cmd.key);
    else if (cmd?.type === 'wide') drawing.camera.wide(cmd.keys);
    else if (cmd?.type === 'hold') drawing.camera.hold();
  }
  if (pinned && !stillThere(pinned)) close();

  drawing.frame(t, { model, lost, hover: pinned ? null : hover, selected: pinned, ring });

  document.body.classList.toggle('lost', !!lost);
  const since0 = model.connection.since;
  banner.update(
    lost === 'stream' ? 'Argus cannot be reached. Showing the last known state, and trying again.'
      : lost === 'cluster' ? `Argus has lost the cluster${model.cluster.since ? ` since ${new Date(model.cluster.since).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}` : ''}. Showing the last known state.`
        : model.snapshots === 0 && t - since0 > 1500 ? 'Connecting to Argus.'
          : model.snapshots > 0 && !model.ready ? 'Argus is still reading the cluster.' : '',
  );
  chip.update(attention.mode, chipText(attention.mode, { showing: attention.showing, resumesIn: attention.resumesIn(t), lost }));

  if (t - lastEdges > 100) {
    lastEdges = t;
    edges.update(attention.waiting().flatMap((e) => {
      const p = drawing.where(e.key);
      return p ? [{ key: e.key, loudness: e.loudness, ...drawing.project(p) }] : [];
    }), drawing.width, drawing.height);
  }
  if (t - lastFeed > 500) {
    lastFeed = t;
    feed.update(model.feed, nowMs);
  }
  const shown = pinned ?? hover;
  if (t - lastCard > 250 || !shown) {
    lastCard = t;
    cardView.show(shown ? cardFor(shown, nowMs) : null, !!pinned, nowMs);
  }
  if (shown) {
    const on = shown.kind === 'env' ? { kind: /** @type {const} */ ('env'), app: shown.app, env: shown.env } : shown;
    const anchor = drawing.placeOf(on);
    if (anchor) {
      const s = drawing.project(anchor);
      cardView.place(s.x, s.y, drawing.screenRadius(on), stageEl.getBoundingClientRect());
    }
  }

  // A GPU that cannot keep 30 frames a second for three seconds gets a
  // lower resolution, once.
  frames++;
  if (t - since >= 1000) {
    const fps = (frames * 1000) / (t - since);
    frames = 0;
    since = t;
    slowSeconds = fps < 30 && document.visibilityState === 'visible' ? slowSeconds + 1 : 0;
    if (slowSeconds >= 3 && t - lastSlow > 10000) {
      lastSlow = t;
      slowSeconds = 0;
      if (drawing.lowerResolution()) console.info('Argus: drawing at a lower resolution to keep up');
    }
  }
  meter?.tick(t, () => {
    const s = drawing.stats();
    return `${s.particles} particles, ${s.lines} lines, ${s.labels} labels, pixel ratio ${drawing.pixelRatio}`;
  });
}
requestAnimationFrame(frame);

// ?debug=1 hands the page's parts to the console, for looking into it.
if (params.get('debug') === '1') Object.assign(globalThis, { argus: { model, attention, drawing, pin, close } });
