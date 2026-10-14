// @ts-check
// The Platform as a particle network: ArgoCD as a core of watching eyes with
// the other components in groups by role round it and k3s as its floor, each
// Application a hub on its orbit with one filament to the core, each
// Environment a cloud of linked particles round its hub, and Deploys
// travelling as beads. Everything is drawn again every frame from the model
// and the times it holds, so a redraw never restarts an animation.

import * as THREE from 'three';
import { CSS2DObject } from 'three/addons/renderers/CSS2DRenderer.js';
import { createParticles, starfield } from './particles.js';
import { eyes } from './eye.js';
import {
  appLayout, bezier, componentPlaces, domainPlace, envKind, lerp, platformReach, postgresPlace, COMPONENT_RADIUS, CORE_RADIUS,
} from '../layout.js';
import { componentLook, envLook, warningTag } from '../look.js';
import { beadsFor } from '../deploys.js';
import { declutter, PRIORITY } from '../labels.js';
import { capabilityKey, componentName } from '../card.js';

/** @typedef {import('../types.js').Vec} Vec */
/** @typedef {import('../types.js').Target} Target */
/** @typedef {import('../types.js').Environment} Environment */
/** @typedef {import('../model.js').Model} Model */
/** @typedef {import('../model.js').AppView} AppView */
/** @typedef {import('../layout.js').AppLayout} AppLayout */
/** @typedef {import('./stage.js').Stage} Stage */

/**
 * What a frame is drawn from: the model, whether the picture is the last
 * known state rather than a live one, what the pointer and the card are
 * on, and the feed entry the pointer is on.
 * @typedef {{model: Model, lost: 'cluster' | 'stream' | null, hover: Target | null, selected: Target | null, ring: string | null}} View
 */

/** The one dark look's colours. */
const COL = {
  prod: '#d4e9ff', staging: '#98bdf8', preview: '#8aa1d4', core: '#ffdf9a', comp: '#eccb82', hub: '#cfe0f5', filament: '#6f93c4',
  arriving: '#5fd3ff', work: '#ffc233', stuck: '#ff7a1a', bad: '#ff5146', grey: '#8b949e', unknown: '#9aa3ad', plot: '#b8c6d6',
  postgres: '#58a6ff', login: '#e5bf57', domain: '#4f9bff', task: '#7ee787', warn: '#ffcc33', backup: '#58a6ff', promote: '#fff0b8',
  signal: '#ff8a80',
};
/** @type {Record<string, THREE.Color>} */
const C = Object.fromEntries(Object.entries({ ...COL, white: '#ffffff' }).map(([k, v]) => [k, new THREE.Color(v)]));
const hot = new THREE.Color();

const clamp01 = (/** @type {number} */ q) => Math.min(1, Math.max(0, q));
const frac = (/** @type {number} */ x) => x - Math.floor(x);
const ease = (/** @type {number} */ q) => (q < 0.5 ? 4 * q * q * q : 1 - Math.pow(-2 * q + 2, 3) / 2);
/** @param {string} s */
const hash = (s) => {
  let h = 2166136261;
  for (const ch of s) h = Math.imul(h ^ ch.charCodeAt(0), 16777619);
  return (h >>> 0) / 4294967295;
};

/** The order Environments are drawn in: prod, staging, then the others by name. */
const envOrder = (/** @type {string} */ a, /** @type {string} */ b) => {
  const rank = (/** @type {string} */ n) => (n === 'prod' ? 0 : n === 'staging' ? 1 : 2);
  return rank(a) - rank(b) || a.localeCompare(b, 'en', { numeric: true });
};

/** A number in [0, 1) that is the same for the same i and k, and looks random. */
const noise = (/** @type {number} */ i, /** @type {number} */ k) => frac(Math.sin(i * 12.9898 + k * 78.233) * 43758.5453);

/**
 * Points spread through a ball: the first is the nucleus at the centre, the
 * rest spread evenly over a sphere along a spiral, then each moved off the
 * spiral and set at its own depth. Without the move, the links between
 * near neighbours trace the spiral's arms, and the cloud looks like a shell.
 */
const directions = new Map();
/** @param {number} n @returns {Vec[]} */
function dirsFor(n) {
  if (!directions.has(n)) {
    const spacing = Math.sqrt((4 * Math.PI) / n);
    /** @type {Vec[]} */
    const out = [];
    for (let i = 0; i < n; i++) {
      const y = 1 - ((i + 0.5) / n) * 2;
      const r = Math.sqrt(1 - y * y);
      const a = i * 2.39996;
      const m = spacing * 0.8;
      const x1 = Math.cos(a) * r + (noise(i, 1) - 0.5) * m;
      const y1 = y + (noise(i, 2) - 0.5) * m;
      const z1 = Math.sin(a) * r + (noise(i, 3) - 0.5) * m;
      const f = i === 0 ? 0 : (0.42 + 0.58 * noise(i, 4)) / (Math.hypot(x1, y1, z1) || 1);
      out.push([x1 * f, y1 * f, z1 * f]);
    }
    directions.set(n, out);
  }
  return directions.get(n);
}

/** A ring texture for the selection and the feed's hover. */
function ringTexture() {
  const c = document.createElement('canvas');
  c.width = c.height = 256;
  const g = /** @type {CanvasRenderingContext2D} */ (c.getContext('2d'));
  g.strokeStyle = '#fff';
  g.lineWidth = 10;
  g.beginPath();
  g.arc(128, 128, 110, 0, Math.PI * 2);
  g.stroke();
  g.globalAlpha = 0.35;
  g.lineWidth = 26;
  g.stroke();
  const t = new THREE.CanvasTexture(c);
  t.colorSpace = THREE.SRGBColorSpace;
  return t;
}

/**
 * @typedef {{
 *   col: string | THREE.Color, alpha?: number, rad?: number, jit?: number, wave?: number, link?: number, speed?: number,
 *   size?: number, sparks?: boolean, freeze?: boolean, fragments?: number, outline?: boolean, squash?: number,
 *   arrive?: {born: number, dur: number, stagger: number, L: AppLayout} | null, disperse?: number
 * }} Style
 */

/**
 * The particle network behind drawing.js: frame(t, view) draws
 * one frame, where(key) says where an Application (or the Platform) is,
 * placeOf(target) where a thing is, and pick(x, y) what is under a point
 * of the screen.
 * @param {Stage} stage
 * @param {{reduced: boolean}} options
 */
export function createNetwork(stage, { reduced }) {
  const motion = reduced ? 0.25 : 1;
  const root = new THREE.Group();
  stage.scene.add(root);
  starfield(stage.scene, stage.pointScale);
  const P = createParticles(root, stage.pointScale, 14000, 48000);

  const ringTex = ringTexture();
  const selection = new THREE.Sprite(new THREE.SpriteMaterial({ map: ringTex, color: '#9fdcff', transparent: true, depthTest: false, depthWrite: false }));
  selection.renderOrder = 31;
  selection.visible = false;
  stage.scene.add(selection);
  const marker = new THREE.Sprite(new THREE.SpriteMaterial({ map: ringTex, color: '#5fd3ff', transparent: true, depthTest: false, depthWrite: false, blending: THREE.AdditiveBlending }));
  marker.renderOrder = 30;
  marker.visible = false;
  stage.scene.add(marker);

  // Each cluster keeps its looks smoothed between frames, so that a change
  // of state glides rather than jumps.
  /** @type {Map<string, {col: THREE.Color, alpha: number, rad: number, jit: number, wave: number, seed: number, seen: number, pos: Vec | null}>} */
  const nodes = new Map();
  let frameNo = 0;
  const tc = new THREE.Color();
  /**
   * @param {string} key @param {Style} st
   */
  function node(key, st) {
    let n = nodes.get(key);
    if (!n) {
      n = { col: new THREE.Color(st.col), alpha: st.alpha ?? 1, rad: st.rad ?? 1, jit: st.jit ?? 0, wave: st.wave ?? 0, seed: hash(key) * 6.283, seen: 0, pos: null };
      nodes.set(key, n);
    }
    n.seen = frameNo;
    n.col.lerp(typeof st.col === 'string' ? tc.set(st.col) : st.col, 0.05);
    n.alpha += ((st.alpha ?? 1) - n.alpha) * 0.06;
    n.rad += ((st.rad ?? 1) - n.rad) * 0.04;
    n.jit += ((st.jit ?? 0) - n.jit) * 0.05;
    n.wave += ((st.wave ?? 0) - n.wave) * 0.05;
    return n;
  }

  /** Where each Application draws, kept until its slot or Environments change. */
  /** @type {Map<string, {key: string, L: AppLayout, filament: Vec[]}>} */
  const layouts = new Map();
  /** @param {AppView} v @param {string[]} names */
  function layoutOf(v, names) {
    const key = `${v.slot}:${names.join(',')}`;
    let rec = layouts.get(v.name);
    if (!rec || rec.key !== key) {
      const L = appLayout(v.slot, names);
      const f = L.filament;
      const len = Math.hypot(f.end[0] - f.start[0], f.end[1] - f.start[1], f.end[2] - f.start[2]) * 1.15;
      const m = Math.max(6, Math.round(len / 1.4));
      const filament = [];
      for (let i = 1; i < m; i++) filament.push(bezier(f.start, f.control, f.end, i / m));
      rec = { key, L, filament };
      layouts.set(v.name, rec);
    }
    return rec;
  }

  // Labels are HTML, so they stay sharp and out of the glow. They are
  // asked for every frame by id; one not asked for is removed.
  /** @type {Map<string, {obj: CSS2DObject, el: HTMLElement, text: string, cls: string, priority: number, near: number, pos: Vec, seen: number, w: number, h: number}>} */
  const labels = new Map();
  /**
   * @param {string} id @param {string} text @param {string} cls @param {Vec} pos
   * @param {number} priority @param {number} [near] fade out beyond this camera distance
   */
  function label(id, text, cls, pos, priority, near = 0) {
    let l = labels.get(id);
    if (!l) {
      const el = document.createElement('div');
      const obj = new CSS2DObject(el);
      root.add(obj);
      l = { obj, el, text: '', cls: '', priority, near, pos, seen: 0, w: 0, h: 0 };
      labels.set(id, l);
    }
    if (l.text !== text) {
      l.el.textContent = text;
      l.text = text;
      l.w = 0;
    }
    if (l.cls !== cls) {
      l.el.className = `lbl ${cls}`;
      l.cls = cls;
      l.w = 0;
    }
    l.priority = priority;
    l.near = near;
    l.pos = pos;
    l.obj.position.set(pos[0], pos[1], pos[2]);
    l.seen = frameNo;
  }

  let lastDeclutter = 0;
  /** @param {number} t @param {View} view */
  function placeLabels(t, view) {
    for (const [id, l] of labels) {
      if (l.seen !== frameNo) {
        l.obj.removeFromParent();
        l.el.remove();
        labels.delete(id);
      }
    }
    if (t - lastDeclutter < 120) return;
    lastDeclutter = t;
    const focus = view.selected ?? view.hover;
    const focusId = focus?.kind === 'env' ? `env:${focus.app}/${focus.env}` : focus?.kind === 'component' ? `comp:${focus.name}` : focus ? 'plat' : '';
    /** @type {import('../labels.js').Box[]} */
    const boxes = [];
    for (const [id, l] of labels) {
      const s = stage.project(l.pos);
      if (!s.front) continue;
      if (!l.w) {
        l.w = l.el.offsetWidth;
        l.h = l.el.offsetHeight;
      }
      const priority = focusId && (id === focusId || id.startsWith(`${focusId}:`)) ? PRIORITY.focused : l.priority;
      boxes.push({ id, x: s.x, y: s.y, w: l.w, h: l.h, priority });
    }
    const shown = declutter(boxes, { width: stage.width, height: stage.height });
    for (const [id, l] of labels) {
      const near = l.near ? clamp01((l.near - stage.distanceTo(l.pos)) / (l.near * 0.25)) : 1;
      const visible = shown.has(id) && near > 0.02;
      l.el.style.visibility = visible ? 'visible' : 'hidden';
      l.el.style.opacity = visible ? String(near) : '0';
    }
  }

  const lostGrey = C.grey;
  const cA = new THREE.Color();
  let lost = false;
  /** @type {{right: Vec, up: Vec}} */
  let basis = { right: [1, 0, 0], up: [0, 1, 0] };

  /**
   * One cluster of particles and its web; the index of its nucleus.
   * @param {string} key @param {Vec} c @param {number} n @param {number} R @param {Style} st @param {number} t
   */
  function cluster(key, c, n, R, st, t) {
    const nd = node(key, st);
    const d = dirsFor(n);
    const freeze = !!st.freeze;
    const spin = nd.seed + (freeze ? 0 : t * 0.00007 * motion);
    const cs = Math.cos(spin);
    const sn = Math.sin(spin);
    const speed = (st.speed ?? 1) * 0.0006;
    const amp = freeze ? 0 : (0.1 + nd.jit * motion + (lost ? 0.25 : 0)) * R * 0.32 * motion;
    const start = P.count;
    const { right, up } = basis;

    if (st.outline) {
      // Unknown: the cloud collapses to a dotted outline, with the last
      // known cloud faint inside it.
      for (let i = 0; i < n; i++) {
        const a = (i / n) * Math.PI * 2 + nd.seed;
        const ca = Math.cos(a) * R * 1.1;
        const sa = Math.sin(a) * R * 1.1;
        cA.copy(nd.col);
        if (lost) cA.lerp(lostGrey, 0.8);
        P.point(c[0] + right[0] * ca + up[0] * sa, c[1] + right[1] * ca + up[1] * sa, c[2] + right[2] * ca + up[2] * sa, cA, 0.95 * nd.alpha, 1.6);
      }
      for (let i = 0; i < n; i += 3) {
        const [x0, y0, z0] = d[i];
        P.point(c[0] + x0 * R * 0.8, c[1] + y0 * R * 0.8, c[2] + z0 * R * 0.8, nd.col, 0.14 * nd.alpha, 1.1);
      }
      return start;
    }

    const fragments = st.fragments ?? 1;
    for (let i = 0; i < n; i++) {
      const [x0, y0, z0] = d[i];
      const x = x0 * cs - z0 * sn;
      const z = x0 * sn + z0 * cs;
      const ph = nd.seed + i * 1.7;
      const tt = t * speed;
      let px = x * R * nd.rad + Math.sin(tt * (1 + frac(i * 0.37)) + ph) * amp;
      let py = y0 * R * nd.rad + Math.sin(tt * (1 + frac(i * 0.53)) + ph * 1.3) * amp;
      let pz = z * R * nd.rad + Math.sin(tt * (1 + frac(i * 0.71)) + ph * 0.7) * amp;
      let a = nd.alpha * (i === 0 ? 0.8 : 1);
      if (fragments > 1 && i > 0) {
        // Degraded: the cloud breaks into pieces that pull apart.
        const g = i % fragments;
        const ga = (g / fragments) * Math.PI * 2 + nd.seed;
        const push = R * (0.42 + 0.06 * Math.sin(t * 0.004 * motion + g));
        const cx = Math.cos(ga) * push;
        const cy = Math.sin(ga) * push;
        px = px * 0.62 + right[0] * cx + up[0] * cy;
        py = py * 0.62 + right[1] * cx + up[1] * cy;
        pz = pz * 0.62 + right[2] * cx + up[2] * cy;
      }
      if (st.sparks && i % 4 === 1) {
        const k = frac((t * motion) / 1300 + i * 0.37);
        const f = 1 + k * 1.1;
        px *= f;
        py *= f;
        pz *= f;
        a *= 1 - k;
      }
      if (st.disperse) {
        const f = 1 + st.disperse * 3.5;
        px *= f;
        py *= f;
        pz *= f;
        a *= 1 - st.disperse;
      }
      if (st.squash) py *= st.squash;
      let X = c[0] + px;
      let Y = c[1] + py;
      let Z = c[2] + pz;
      if (st.arrive) {
        // Arriving: particles leave the core one by one along the
        // filament, then settle into the new cloud.
        const ar = st.arrive;
        const q = clamp01((t - ar.born - i * ar.stagger) / ar.dur);
        if (q <= 0) a = 0;
        else if (q < 1) {
          const s = ease(q);
          const f = ar.L.filament;
          if (s < 0.6) {
            const p = bezier(f.start, f.control, f.end, s / 0.6);
            X = p[0] + px * 0.15;
            Y = p[1] + py * 0.15;
            Z = p[2] + pz * 0.15;
          } else {
            const k = (s - 0.6) / 0.4;
            X = f.end[0] + (X - f.end[0]) * k;
            Y = f.end[1] + (Y - f.end[1]) * k;
            Z = f.end[2] + (Z - f.end[2]) * k;
          }
        }
      }
      cA.copy(nd.col);
      if (nd.wave > 0.01) cA.lerp(C.work, nd.wave * (0.5 + 0.5 * Math.sin((t * motion) / 240 - y0 * 4)));
      if (lost) cA.lerp(lostGrey, 0.8);
      P.point(X, Y, Z, cA, a, (i === 0 ? 2.2 : i % 9 === 0 ? 2.5 : 1.3) * (st.size ?? 1));
    }
    const flicker = st.sparks ? 0.55 + 0.45 * Math.sin((t * motion) / 70 + nd.seed) : 1;
    P.web(start, P.count, R * 0.62 * Math.max(1, nd.rad * 0.85) * (fragments > 1 ? 0.75 : 1), (st.link ?? 1) * (lost ? 0.12 : 1) * 0.55 * flicker, fragments);
    return start;
  }

  /**
   * A hard, solid ring round something, facing the camera: a stuck cloud
   * freezes behind one.
   * @param {Vec} c @param {number} r @param {THREE.Color} col @param {number} alpha
   */
  function hardRing(c, r, col, alpha) {
    const n = 44;
    const start = P.count;
    const { right, up } = basis;
    cA.copy(col);
    if (lost) cA.lerp(lostGrey, 0.8);
    for (let i = 0; i < n; i++) {
      const a = (i / n) * Math.PI * 2;
      const ca = Math.cos(a) * r;
      const sa = Math.sin(a) * r;
      P.point(c[0] + right[0] * ca + up[0] * sa, c[1] + right[1] * ca + up[1] * sa, c[2] + right[2] * ca + up[2] * sa, cA, alpha * 0.9, 1.3);
    }
    for (let i = 0; i < n; i++) P.link(start + i, start + ((i + 1) % n), alpha * 0.85);
  }

  /**
   * A reserved plot: a thin broken ring of particles with nothing inside.
   * @param {string} key @param {Vec} c @param {number} R @param {number} alpha @param {number} t
   */
  function plot(key, c, R, alpha, t) {
    const nd = node(key, { col: COL.plot, alpha });
    const n = 14;
    const s = P.count;
    for (let i = 0; i < n; i++) {
      const a = (i / n) * Math.PI * 2 + t * 0.0001 * motion;
      P.point(c[0] + Math.cos(a) * R, c[1], c[2] + Math.sin(a) * R, nd.col, nd.alpha * 0.55, 1.2);
    }
    for (let i = 0; i < n; i += 2) P.link(s + i, s + i + 1, nd.alpha * 0.45);
    return s;
  }

  /** @type {{target: Target, pos: Vec, R: number}[]} */
  let picks = [];
  /** @type {Map<string, Vec>} */
  let places = new Map();
  /** @type {Map<string, Vec>} Where each Environment's cloud is now, smoothed. */
  const envPos = new Map();
  /** @type {Map<string, Vec>} Where each Capability is now. */
  const capPos = new Map();

  /**
   * An Environment's Capabilities on its cloud.
   * @param {Environment} env @param {Vec} E @param {number} R @param {AppLayout} L @param {number} nuc
   * @param {number} ea @param {number} t @param {string} app
   */
  function capabilities(env, E, R, L, nuc, ea, t, app) {
    for (const c of env.capabilities) {
      const bad = c.condition.state === 'Degraded';
      const warn = !!c.condition.warning;
      const blink = frac((t * Math.max(motion, 0.5)) / 700) < 0.5;
      const ca = ea * (c.activity ? 0.55 : 1);
      if (c.type === 'postgres') {
        const p = postgresPlace(L, E, R);
        const col = bad ? COL.bad : warn ? (blink ? COL.warn : COL.postgres) : COL.postgres;
        P.link(nuc, cluster(`pg:${app}/${env.name}/${c.name}`, p, 8, 0.45, { col, alpha: ca }, t), 0.4 * ca);
        capPos.set(`${app}/${env.name}/${capabilityKey(c)}`, p);
        picks.push({ target: { kind: 'env', app, env: env.name, cap: capabilityKey(c) }, pos: p, R: 0.7 });
        const tag = warningTag(c);
        if (tag) label(`env:${app}/${env.name}:warn:${c.name}`, tag, 'tag warn', [p[0], p[1] - 0.9, p[2]], PRIORITY.tag);
      } else if (c.type === 'itema-login') {
        // A slowly turning halo of gold round the Environment.
        const s = P.count;
        const n = 14;
        const rr = R * 1.3;
        for (let i = 0; i < n; i++) {
          const a = (i / n) * Math.PI * 2 + t * 0.00035 * motion;
          const x = Math.cos(a);
          const z = Math.sin(a);
          P.point(E[0] + (L.tangent[0] * x + L.radial[0] * z) * rr, E[1] + z * 0.35, E[2] + (L.tangent[2] * x + L.radial[2] * z) * rr, bad ? C.bad : C.login, 0.6 * ca, 1.0);
        }
        for (let i = 0; i < n; i++) P.link(s + i, s + ((i + 1) % n), 0.22 * ca);
      } else if (c.type === 'custom-domain') {
        const p = domainPlace(L, E, R);
        P.link(nuc, P.point(p[0], p[1], p[2], bad ? C.bad : warn && blink ? C.warn : C.domain, ca, 3.4), 0.4 * ca);
        capPos.set(`${app}/${env.name}/${capabilityKey(c)}`, p);
        picks.push({ target: { kind: 'env', app, env: env.name, cap: capabilityKey(c) }, pos: p, R: 0.55 });
        label(`env:${app}/${env.name}:domain:${c.name}`, warn ? `! ${c.name}` : c.name, warn ? 'tag warn' : 'tag domain', [p[0], p[1] + 0.55, p[2]], PRIORITY.detail, 110);
      } else if (c.type === 'scheduled-task') {
        // A mote circling like a clock hand, trailing its last positions.
        const w = ((t * motion) / 3500) * Math.PI * 2 + hash(c.name) * 6;
        const rr = R + 0.75;
        let prev = -1;
        for (let s = 0; s < 7; s++) {
          const ww = w - s * 0.11;
          const col = warn && blink ? C.warn : C.task;
          const i = P.point(E[0] + L.tangent[0] * Math.cos(ww) * rr, E[1] + Math.sin(ww) * rr, E[2] + L.tangent[2] * Math.cos(ww) * rr, col, ca * (1 - s / 7), s ? 1.8 - s * 0.2 : 2.8);
          if (prev >= 0) P.link(prev, i, 0.45 * ca * (1 - s / 7));
          prev = i;
        }
        if (warn) label(`env:${app}/${env.name}:warn:${c.name}`, warningTag(c) ?? '!', 'tag warn', [E[0], E[1] - R - 1.4, E[2]], PRIORITY.tag);
      }
    }
  }

  /**
   * The beads of an Environment's Deploys.
   * @param {AppView & {fade: number}} v @param {Environment} env @param {AppLayout} L @param {Vec} E @param {Model} model @param {number} t
   */
  function beads(v, env, L, E, model, t) {
    const list = beadsFor(env, model.deployLife(v.name, env.name), t);
    if (!list.length) return;
    const staging = envPos.get(`${v.name}/staging`) ?? L.centre;
    const f = L.filament;
    /** @param {string} w @returns {Vec} */
    const at = (w) => (w === 'gate' ? places.get('deploy-gate') ?? [0, 0, 0] : w === 'core' ? [0, 0, 0] : w === 'coreEdge' ? f.start : w === 'hub' ? L.centre : w === 'staging' ? staging : E);
    /** @param {string[]} route @param {number} k @returns {Vec} */
    const on = (route, k) => {
      const legs = route.length - 1;
      if (legs <= 0) return at(route[0]);
      const q = clamp01(k) * legs;
      const seg = Math.min(legs - 1, Math.floor(q));
      const u = q - seg;
      const a = route[seg];
      const b = route[seg + 1];
      if (a === 'coreEdge' && b === 'hub') return bezier(f.start, f.control, f.end, u);
      if (a === 'hub' && b === 'coreEdge') return bezier(f.start, f.control, f.end, 1 - u);
      const p = lerp(at(a), at(b), u);
      if (a === 'gate' || a === 'staging') p[1] += Math.sin(u * Math.PI) * 4.5;
      return p;
    };
    for (const b of list) {
      const col = b.tone === 'stuck' ? C.stuck : b.tone === 'refused' ? C.bad : b.tone === 'backup' ? C.backup : b.tone === 'promote' ? C.promote : C.work;
      cA.copy(col);
      if (lost) cA.lerp(lostGrey, 0.6);
      if (b.flash > 0) {
        // Serving: one flash in the cloud.
        const R = envKind(env.name) === 'prod' ? 1.9 : envKind(env.name) === 'staging' ? 1.5 : 0.95;
        P.point(E[0], E[1], E[2], cA, b.flash, 6 + 16 * (1 - b.flash));
        const s = P.count;
        const n = 18;
        const rr = R * (1 + (1 - b.flash) * 1.4);
        for (let i = 0; i < n; i++) {
          const a = (i / n) * Math.PI * 2;
          const ca = Math.cos(a) * rr;
          const sa = Math.sin(a) * rr;
          P.point(E[0] + basis.right[0] * ca + basis.up[0] * sa, E[1] + basis.right[1] * ca + basis.up[1] * sa, E[2] + basis.right[2] * ca + basis.up[2] * sa, cA, b.flash * 0.8, 1.4);
        }
        for (let i = 0; i < n; i++) P.link(s + i, s + ((i + 1) % n), b.flash * 0.6);
        continue;
      }
      const p = on(b.route, b.k);
      const bl = 0.5 + 0.5 * Math.sin(t / 110);
      const alpha = b.alpha * (b.blink ? 0.55 + 0.45 * bl : 1) * v.fade;
      const size = b.blink ? 7 + 3 * bl : b.tone === 'backup' ? 3.4 : 8;
      // A bead is a hot white centre in a halo of its colour, so that it
      // shows against the core's glow as well as against the dark.
      P.point(p[0], p[1], p[2], cA, alpha * 0.7, size);
      if (b.tone !== 'backup') P.point(p[0], p[1], p[2], hot.copy(cA).lerp(C.white, 0.65), alpha, size * 0.42);
      if (b.pulse) {
        // Waiting, or holding at the hub: a ring breathing round it.
        const k = frac(t / 1400);
        const rr = 0.5 + k * 1.6;
        const s = P.count;
        for (let i = 0; i < 10; i++) {
          const a = (i / 10) * Math.PI * 2;
          const ca = Math.cos(a) * rr;
          const sa = Math.sin(a) * rr;
          P.point(p[0] + basis.right[0] * ca + basis.up[0] * sa, p[1] + basis.right[1] * ca + basis.up[1] * sa, p[2] + basis.right[2] * ca + basis.up[2] * sa, cA, alpha * (1 - k), 1.6);
        }
        for (let i = 0; i < 10; i++) P.link(s + i, s + ((i + 1) % 10), alpha * (1 - k) * 0.7);
      }
      if (b.trail) {
        for (let s = 1; s < 9; s++) {
          const kk = b.k - s * 0.022;
          if (kk < 0) break;
          const q = on(b.route, kk);
          P.point(q[0], q[1], q[2], cA, alpha * (1 - s / 9), 2.8 - s * 0.25);
        }
      }
      if (b.tone === 'refused' && b.k >= 1) {
        // It fizzles out at the core.
        const spread = (1 - b.alpha) * 2.2;
        for (let i = 0; i < 8; i++) {
          const a = i * 0.785 + hash(env.name);
          P.point(p[0] + Math.cos(a) * spread, p[1] + Math.sin(a * 2) * spread * 0.5, p[2] + Math.sin(a) * spread, cA, b.alpha * 0.8, 1.6);
        }
      }
    }
  }

  const appColour = { prod: C.prod, staging: C.staging, preview: C.preview };

  /**
   * One Environment's cloud, its look, its Capabilities and its labels.
   * @param {AppView & {fade: number}} v @param {AppLayout} L @param {AppLayout['envs'][number]} spot
   * @param {Environment} env @param {number} hub @param {Model} model @param {number} t
   */
  function environment(v, L, spot, env, hub, model, t) {
    const key = `${v.name}/${env.name}`;
    const life = model.envLife(v.name, env.name);
    const kind = spot.kind;
    const look = envLook(env, { newApp: model.isNewApp(v.name), preview: kind === 'preview' });
    // The cloud glides to its place when its neighbours change.
    let E = envPos.get(key);
    if (!E) E = /** @type {Vec} */ ([...spot.centre]);
    else E = lerp(E, spot.centre, 0.06);
    envPos.set(key, E);
    const R = spot.R;
    const gone = life?.gone ?? 0;
    const fade = v.fade;

    if (look.shape === 'plot' || (life?.wasUnreleased && env.activity?.state === 'Leaving')) {
      const alpha = fade * (gone ? 1 - clamp01((t - gone) / 900) : env.activity?.state === 'Leaving' ? 0.6 : 1);
      P.link(hub, plot(`env:${key}`, E, R, alpha, t), 0.2 * alpha);
    } else {
      /** @type {Style} */
      const st = { col: appColour[kind], alpha: 1, rad: 1, jit: 0, wave: 0, link: 1 };
      if (look.wave) st.wave = 0.85;
      switch (look.tone) {
        case 'degraded':
          Object.assign(st, { col: COL.bad, jit: 0.4, speed: 4, sparks: true, rad: 1.05, fragments: 3 });
          break;
        case 'unknown':
          Object.assign(st, { col: COL.unknown, alpha: 0.85, outline: true, wave: 0 });
          break;
        case 'stuck':
          Object.assign(st, { col: COL.stuck, wave: 0, freeze: true });
          break;
        case 'arriving':
          st.col = COL.arriving;
          if (life && life.born > -Infinity) st.arrive = { born: life.born, dur: 3600 / Math.max(motion, 0.5), stagger: 2600 / spot.n, L };
          break;
        case 'leaving': {
          const q = clamp01((t - (life?.leaving || t)) / 3500);
          Object.assign(st, { col: COL.grey, alpha: 1 - 0.55 * q, rad: 1 + 0.3 * q, link: 1 - 0.7 * q, wave: 0 });
          break;
        }
      }
      if (gone) st.disperse = clamp01((t - gone) / 1300);
      st.alpha = (st.alpha ?? 1) * fade;
      const nuc = cluster(`env:${key}`, E, spot.n, R, st, t);
      const ea = /** @type {{alpha: number}} */ (nodes.get(`env:${key}`)).alpha;
      P.link(hub, nuc, 0.35 * ea);
      if (look.shape === 'ring') {
        hardRing(E, R * 1.55, C.stuck, ea);
        const bl = 0.5 + 0.5 * Math.sin(t / 110);
        P.point(E[0], E[1] + R + 0.4, E[2], C.stuck, 0.5 + 0.5 * bl, 4 + 3 * bl);
      }
      if (!gone && look.tone !== 'leaving') {
        const arrived = look.tone === 'arriving' && life && life.born > -Infinity ? clamp01((t - life.born - 3000) / 1500) : 1;
        capabilities(env, E, R, L, nuc, ea * arrived, t, v.name);
      }
    }
    if (!gone) {
      beads(v, env, L, E, model, t);
      picks.push({ target: { kind: 'env', app: v.name, env: env.name }, pos: E, R: R * 1.15 });
      label(`env:${key}`, env.name, 'env', [E[0], E[1] - R - 0.5, E[2]], PRIORITY.environment, 70);
      if (look.tag) {
        const lift = env.capabilities.some((c) => c.type === 'custom-domain') ? 1.9 : 0;
        const loud = look.tone === 'stuck' || look.tone === 'degraded' || look.tone === 'unknown';
        label(`env:${key}:tag`, look.tag.text, `tag ${look.tag.tone}`, [E[0], E[1] + R + 1.0 + lift, E[2]], loud ? PRIORITY.loudTag : PRIORITY.tag);
      }
    }
  }

  // Where the core's eyes look, eased towards what they look at.
  /** @type {Vec} */
  const gaze = [0, 0, 30];
  const coreCol = new THREE.Color();
  const coreHot = new THREE.Color();
  /**
   * What the core's eyes look at: what the pointer or the card is on, the
   * viewer when that is the core itself, else an Environment at work, else
   * each Application in turn.
   * @param {number} t @param {View} view @returns {Vec}
   */
  function gazeTarget(t, view) {
    const sel = view.selected ?? view.hover;
    const viewer = stage.camera.position;
    if (sel?.kind === 'core') return [viewer.x, viewer.y, viewer.z];
    const p = sel ? placeOf(sel) : null;
    if (p) return p;
    for (const v of view.model.apps.values()) {
      for (const e of v.app.environments) {
        const s = e.activity?.state;
        const at = s === 'Deploying' || s === 'Updating' || s === 'Arriving' ? envPos.get(`${v.name}/${e.name}`) : null;
        if (at) return at;
      }
    }
    const names = [...view.model.apps.keys()].sort();
    return (names.length && where(names[Math.floor(t / 5000) % names.length])) || [viewer.x, viewer.y, viewer.z];
  }

  /**
   * Draws one frame.
   * @param {number} t
   * @param {View} view
   */
  function frame(t, view) {
    const { model } = view;
    frameNo++;
    lost = !!view.lost;
    basis = stage.basis();
    P.begin();
    picks = [];

    // The core: ArgoCD as a ball of eyes. They ripple shut and open while
    // it works, shake red when it is Degraded, freeze orange when it is
    // stuck, and stay closed while its state is Unknown.
    const argo = model.components.get('argocd');
    const coreLook = argo ? componentLook(argo) : null;
    const tone = coreLook?.tone;
    const nd = node('core', {
      col: tone === 'degraded' ? COL.bad : tone === 'stuck' ? COL.stuck : tone === 'unknown' ? COL.unknown : COL.core,
      jit: tone === 'degraded' ? 1 : 0,
    });
    const to = gazeTarget(t, view);
    if (tone !== 'stuck') for (let i = 0; i < 3; i++) gaze[i] += (to[i] - gaze[i]) * 0.035;
    coreCol.copy(nd.col);
    coreHot.copy(nd.col).lerp(C.white, tone === 'degraded' || tone === 'stuck' ? 0.35 : 0.55);
    if (lost) {
      coreCol.lerp(lostGrey, 0.8);
      coreHot.lerp(lostGrey, 0.8);
    }
    const core = eyes({
      P, c: [0, 0, 0], R: CORE_RADIUS, col: coreCol, hot: coreHot, alpha: 0.6, t, motion, basis, gaze,
      open: tone === 'unknown' ? 0.04 : 1, dilate: coreLook?.wave ? 1 : 0, jit: nd.jit, freeze: tone === 'stuck',
    });
    if (coreLook?.shape === 'ring') hardRing([0, 0, 0], CORE_RADIUS * 1.2, C.stuck, 0.8);
    picks.push({ target: { kind: 'core' }, pos: [0, 0, 0], R: CORE_RADIUS * 0.9 });
    label('plat', 'PLATFORM', 'plat', [0, 5.6, 0], PRIORITY.application);
    label('comp:argocd', 'ArgoCD', 'comp', [0, -3.4, 0], PRIORITY.group, 90);
    if (coreLook?.tag) label('comp:argocd:tag', coreLook.tag.text, `tag ${coreLook.tag.tone}`, [0, 6.6, 0], PRIORITY.loudTag);

    // The Platform's components in their groups, and k3s as the floor. A
    // member's name shows when the camera is near, when the pointer or the
    // card is on it, or when it is not Healthy. The Deploy gate lights up
    // while a Deploy passes it.
    const names = [...model.components.keys()].sort();
    const placed = componentPlaces(names);
    places = placed.places;
    let gateBusy = false;
    for (const v of model.apps.values()) for (const e of v.app.environments) if (e.activity?.deploy?.hop === 'Accepted' && !e.activity.deploy.promote) gateBusy = true;
    const focus = view.selected ?? view.hover;
    /** @param {string} name @param {number} at the nucleus to link to */
    const component = (name, at) => {
      const c = /** @type {import('../types.js').Component} */ (model.components.get(name));
      const p = /** @type {Vec} */ (places.get(name));
      const look = componentLook(c);
      const gate = name === 'deploy-gate' && gateBusy;
      const floor = name === 'k3s';
      /** @type {Style} */
      const st = {
        col: look.tone === 'stuck' ? COL.stuck : look.tone === 'degraded' ? COL.bad : look.tone === 'unknown' ? COL.unknown : COL.comp,
        wave: look.wave ? 0.85 : 0, jit: look.tone === 'degraded' ? 0.55 : 0, speed: look.tone === 'degraded' ? 4 : 1,
        sparks: look.tone === 'degraded', fragments: look.tone === 'degraded' ? 3 : 1, outline: look.tone === 'unknown',
        freeze: look.tone === 'stuck', size: gate ? 1.5 : 1, squash: floor ? 0.3 : 0,
      };
      const R = floor ? 2.2 : COMPONENT_RADIUS;
      P.link(at, cluster(`comp:${name}`, p, floor ? 28 : 12, R, st, t), gate ? 0.75 : 0.22);
      if (look.shape === 'ring') hardRing(p, R * 1.6, C.stuck, 0.9);
      if (name === 'monitoring') {
        // Signals rising out to Grafana Cloud.
        for (let i = 0; i < 3; i++) {
          const k = frac((t * motion) / 2100 + i / 3);
          P.point(p[0], p[1] + 1 + k * 4, p[2], C.signal, (1 - k) * 0.8, 2);
        }
      }
      picks.push({ target: { kind: 'component', name }, pos: p, R: floor ? R : 0.9 });
      const loud = look.tone !== 'healthy' || (focus?.kind === 'component' && focus.name === name);
      // The name below and the tag above, far enough apart that neither
      // hides the other.
      label(`comp:${name}`, componentName(name), 'comp', [p[0], p[1] - (floor ? 1.2 : 1.0), p[2]], loud ? PRIORITY.tag : PRIORITY.component, floor || loud ? 0 : 30);
      if (look.tag) label(`comp:${name}:tag`, look.tag.text, `tag ${look.tag.tone}`, [p[0], p[1] + (floor ? -3.2 : 1.1), p[2]], PRIORITY.loudTag);
    };
    for (const g of placed.groups) {
      const gate = gateBusy && g.members.includes('deploy-gate');
      const hub = P.point(g.centre[0], g.centre[1], g.centre[2], lost ? lostGrey : C.comp, 0.5, 1.4);
      P.link(core, hub, gate ? 0.75 : 0.16);
      for (const name of g.members) component(name, hub);
      label(`group:${g.key}`, g.title, 'group', [g.centre[0], g.centre[1] + 2.4, g.centre[2]], PRIORITY.group);
    }
    if (places.has('k3s')) component('k3s', core);

    // The Applications.
    for (const view0 of model.apps.values()) {
      const v = /** @type {AppView & {fade: number}} */ (view0);
      const envs = [...v.app.environments, ...v.ghosts].sort((a, b) => envOrder(a.name, b.name));
      const { L, filament } = layoutOf(v, envs.map((e) => e.name));
      v.fade = v.gone ? 1 - clamp01((t - v.gone - 600) / 1800) : 1;
      const planned = v.app.environments.length > 0 && v.app.environments.every((e) => e.activity?.state === 'Unreleased');
      // The filament: the only line out of an Application, and it goes to the core.
      const fa = (planned ? 0.2 : 0.42) * v.fade * (lost ? 0.35 : 1);
      const first = P.count;
      for (let i = 0; i < filament.length; i++) {
        const q = filament[i];
        P.point(q[0], q[1] + Math.sin((t * motion) / 900 + i) * 0.08, q[2], lost ? lostGrey : C.filament, fa, 1.0);
      }
      P.link(core, first, fa * 0.5);
      for (let i = 0; i < filament.length - 1; i++) if (!planned || i % 2 === 0) P.link(first + i, first + i + 1, fa * 0.8);
      const c = L.centre;
      const hub = P.point(c[0], c[1], c[2], lost ? lostGrey : C.hub, 0.85 * v.fade, 2.6);
      P.link(first + filament.length - 1, hub, fa);
      if (!v.gone) label(`app:${v.name}`, v.name, 'app', [c[0], c[1] - 0.9, c[2]], PRIORITY.application);
      if (L.more > 0) label(`app:${v.name}:more`, `+${L.more} previews`, 'env', [c[0], c[1] - 3.2, c[2] + 0.5], PRIORITY.detail, 90);

      for (const spot of L.envs) {
        const env = envs.find((e) => e.name === spot.name);
        if (env) environment(v, L, spot, env, hub, model, t);
      }

      // A new Application sends rings of light out across the dark.
      if (model.isNewApp(v.name)) {
        for (const w of [0, 0.5]) {
          const k = frac((t * motion) / 1800 + w);
          const s = P.count;
          const n = 48;
          const rr = 3 + k * 8;
          for (let i = 0; i < n; i++) {
            const an = (i / n) * Math.PI * 2;
            P.point(c[0] + Math.cos(an) * rr, c[1], c[2] + Math.sin(an) * rr, C.arriving, 0.8 * (1 - k), 1.3);
          }
          for (let i = 0; i < n; i++) P.link(s + i, s + ((i + 1) % n), 0.5 * (1 - k));
        }
      }
    }
    for (const [k, n] of nodes) if (n.seen !== frameNo) nodes.delete(k);
    for (const k of layouts.keys()) if (!model.apps.has(k)) layouts.delete(k);
    for (const k of envPos.keys()) {
      const [app, env] = k.split('/');
      const v = model.apps.get(app);
      if (!v || (!v.app.environments.some((e) => e.name === env) && !v.ghosts.some((e) => e.name === env))) envPos.delete(k);
    }
    P.end();

    // A ring round what the card or the pointer is on, and round the
    // place the pointer is on in the feed.
    const sel = view.selected ?? view.hover;
    const sp = sel ? placeOf(sel) : null;
    selection.visible = !!sp;
    if (sp && sel) {
      const size = sel.kind === 'core' ? 7.5 : sel.kind === 'component' ? (sel.name === 'k3s' ? 6 : 2.2) : sel.cap ? 2 : envKind(sel.env) === 'preview' ? 3.2 : envKind(sel.env) === 'staging' ? 4.6 : 5.6;
      selection.position.set(...sp);
      selection.scale.set(size, size, 1);
      selection.material.opacity = view.selected ? 0.16 : 0.08 + 0.04 * Math.sin(t / 200);
    }
    const mp = view.ring ? where(view.ring) : null;
    marker.visible = !!mp;
    if (mp) {
      const size = view.ring === 'platform' ? 26 : 17;
      marker.position.set(...mp);
      marker.scale.set(size, size, 1);
      marker.material.opacity = 0.5 + 0.4 * Math.sin(t / 180);
    }
    placeLabels(t, view);
  }

  /**
   * Where an Application's hub is, or the Platform's core; null for an
   * Application no longer on the map.
   * @param {string} key an Application's name, or "platform"
   * @returns {Vec | null}
   */
  function where(key) {
    if (key === 'platform') return [0, 0, 0];
    const rec = layouts.get(key);
    return rec ? rec.L.centre : null;
  }

  /**
   * Where a thing is now.
   * @param {Target} target
   * @returns {Vec | null}
   */
  function placeOf(target) {
    switch (target.kind) {
      case 'core':
        return [0, 0, 0];
      case 'component':
        return places.get(target.name) ?? null;
      default:
        if (target.cap) return capPos.get(`${target.app}/${target.env}/${target.cap}`) ?? envPos.get(`${target.app}/${target.env}`) ?? null;
        return envPos.get(`${target.app}/${target.env}`) ?? null;
    }
  }

  return {
    frame,
    where,
    placeOf,

    /**
     * How far a thing reaches on the screen from where it is, in CSS
     * pixels, so that a card can sit beside it rather than on it.
     * @param {Target} target
     */
    screenRadius(target) {
      const p = placeOf(target);
      if (!p) return 0;
      const R = target.kind === 'core' ? CORE_RADIUS : target.kind === 'component' ? (target.name === 'k3s' ? 2.2 : 0.8) : target.cap ? 0.6 : envKind(target.env) === 'prod' ? 2.3 : envKind(target.env) === 'staging' ? 1.9 : 1.3;
      return stage.pixels(p, R);
    },

    /**
     * What is under a point of the stage, in CSS pixels: the nearest
     * thing whose outline on screen holds the point.
     * @param {number} x @param {number} y
     * @returns {Target | null}
     */
    pick(x, y) {
      /** @type {Target | null} */
      let best = null;
      let score = Infinity;
      for (const p of picks) {
        const s = stage.project(p.pos);
        if (!s.front) continue;
        const r = Math.max(stage.pixels(p.pos, p.R), 9);
        const d = Math.hypot(s.x - x, s.y - y);
        if (d > r) continue;
        const q = d / r - (p.target.kind === 'env' && p.target.cap ? 0.35 : 0);
        if (q < score) {
          score = q;
          best = p.target;
        }
      }
      return best;
    },

    /** How far out the whole Platform reaches. @param {Model} model */
    reach(model) {
      return platformReach([...model.apps.values()].map((v) => v.slot));
    },

    stats() {
      return { particles: P.count, lines: P.lineCount, labels: labels.size };
    },
  };
}

/** @typedef {ReturnType<typeof createNetwork>} Network */
