// @ts-check
// The Platform's core as Argus Panoptes, the giant of a hundred eyes: a
// ball covered in eyes that each blink in their own time and all look at
// the same thing.

/** @typedef {import('../types.js').Vec} Vec */
/** @typedef {import('three').Color} Color */

/**
 * What the eyes are drawn from: where the ball is and how far the core
 * reaches, its colours, the camera's right and up, the point every eye
 * looks at, and the core's state as how open the eyes are, how wide their
 * pupils, how much they shake and whether they are frozen.
 * @typedef {{
 *   P: ReturnType<typeof import('./particles.js').createParticles>,
 *   c: Vec, R: number, col: Color, hot: Color, alpha: number,
 *   t: number, motion: number, basis: {right: Vec, up: Vec}, gaze: Vec,
 *   open: number, dilate: number, jit: number, freeze: boolean,
 * }} EyesArgs
 */

const frac = (/** @type {number} */ x) => x - Math.floor(x);
const clamp01 = (/** @type {number} */ q) => Math.min(1, Math.max(0, q));
/** @param {Vec} a @param {Vec} b @returns {Vec} */
const cross = (a, b) => [a[1] * b[2] - a[2] * b[1], a[2] * b[0] - a[0] * b[2], a[0] * b[1] - a[1] * b[0]];
/** @param {Vec} a @param {Vec} b */
const dot = (a, b) => a[0] * b[0] + a[1] * b[1] + a[2] * b[2];
/** @param {Vec} a @returns {Vec} */
const norm = (a) => {
  const l = Math.hypot(a[0], a[1], a[2]) || 1;
  return [a[0] / l, a[1] / l, a[2] / l];
};

/** A number in [0, 1) that is the same for the same i and k, and looks random. */
const noise = (/** @type {number} */ i, /** @type {number} */ k) => frac(Math.sin(i * 12.9898 + k * 78.233) * 43758.5453);

/**
 * Points spread evenly over a sphere along a spiral. With scatter, each
 * moves off the spiral by up to that share of the spacing between points,
 * so that the links between near neighbours do not trace the spiral's arms.
 * @param {number} n @param {number} [scatter] @returns {Vec[]}
 */
function sphere(n, scatter = 0) {
  const m = Math.sqrt((4 * Math.PI) / n) * scatter;
  /** @type {Vec[]} */
  const out = [];
  for (let i = 0; i < n; i++) {
    const y = 1 - ((i + 0.5) / n) * 2;
    const r = Math.sqrt(1 - y * y);
    const a = i * 2.39996;
    out.push(norm([Math.cos(a) * r + (noise(i, 1) - 0.5) * m, y + (noise(i, 2) - 0.5) * m, Math.sin(a) * r + (noise(i, 3) - 0.5) * m]));
  }
  return out;
}
const SHELL = sphere(75, 0.8);
const EYES = sphere(18);

/**
 * Across and up on the sphere at n, as seen from outside, with up as near
 * the world's up as it gets.
 * @param {Vec} n @returns {{across: Vec, upward: Vec}}
 */
function tangent(n) {
  const ref = Math.abs(n[1]) > 0.96 ? /** @type {Vec} */ ([0, 0, -1]) : /** @type {Vec} */ ([0, 1, 0]);
  const upward = norm([ref[0] - n[0] * dot(ref, n), ref[1] - n[1] * dot(ref, n), ref[2] - n[2] * dot(ref, n)]);
  return { across: cross(upward, n), upward };
}

/**
 * How open an eye is with its blinks: shut for a moment every 8 to 13
 * seconds. A frozen eye, or one drawn with reduced motion, never blinks.
 * @param {EyesArgs} o @param {number} seed
 */
function blink(o, seed) {
  if (o.freeze || o.motion < 1) return 1;
  const period = 8 + 5 * frac(seed * 3.7);
  const p = (o.t / 1000 + seed * 11) % period;
  return p < 0.28 ? Math.abs(Math.cos((p / 0.28) * Math.PI)) : 1;
}

/** How far a Degraded eye's point i is shaken. @param {EyesArgs} o @param {number} i */
const shake = (o, i) => (o.jit ? Math.sin(o.t * 0.02 + i * 2.3) * o.jit * o.motion * 0.25 : 0);

/**
 * Draws the ball and its eyes, and returns the index of an unseen particle
 * at the centre for the lines that run to the core. Each eye wraps round
 * the ball, its lids raised off it and its iris domed like a cornea; the
 * eyes on the far side fade.
 * @param {EyesArgs} o
 */
export function eyes(o) {
  const { P, c, t, motion } = o;
  // The ball; the eyes' lids rise to about o.R.
  const R = o.R * 0.72;
  const { right, up } = o.basis;
  const fwd = cross(right, up);
  const nucleus = P.point(c[0], c[1], c[2], o.col, 0, 1);

  const s0 = P.count;
  for (const n of SHELL) P.point(c[0] + n[0] * R * 0.9, c[1] + n[1] * R * 0.9, c[2] + n[2] * R * 0.9, o.col, o.alpha * 0.3, 0.8);
  P.web(s0, P.count, R * 0.7, 0.22 * o.alpha);

  // Degraded: sparks fly off the ball.
  if (o.jit > 0.05) {
    for (let i = 0; i < SHELL.length; i += 4) {
      const n = SHELL[i];
      const k = frac((t * motion) / 1300 + i * 0.37);
      const r = R * (1 + k * 1.1);
      P.point(c[0] + n[0] * r, c[1] + n[1] * r, c[2] + n[2] * r, o.hot, o.alpha * o.jit * (1 - k), 1.1);
    }
  }

  const spin = o.freeze ? 0 : t * 0.00005 * motion;
  const cs = Math.cos(spin);
  const sn = Math.sin(spin);
  for (let e = 0; e < EYES.length; e++) {
    const [x0, y, z0] = EYES[e];
    /** @type {Vec} */
    const n = [x0 * cs - z0 * sn, y, x0 * sn + z0 * cs];
    const a = o.alpha * (0.08 + 0.92 * clamp01(dot(n, fwd) * 1.6 + 0.2));
    const { across, upward } = tangent(n);
    // A Platform at work blinks in a ripple round the ball, unless motion
    // is reduced.
    const ripple = o.dilate > 0.5 && !o.freeze && o.motion >= 1 ? clamp01(Math.abs(Math.sin(t / 500 - e * 0.45)) * 1.4 - 0.1) : 1;
    const open = o.open * blink(o, frac(e * 0.618 + 0.37) * 9) * ripple;
    const w = R * 0.36;
    const h = R * 0.17;
    /**
     * A point of the eye at (u, v) across and up from its centre, wrapped
     * round the ball and lifted off it by lift.
     * @param {number} u @param {number} v @param {number} lift @param {Color} col @param {number} al @param {number} s
     */
    const put = (u, v, lift, col, al, s) => {
      const d = norm([n[0] + (across[0] * u + upward[0] * v) / R, n[1] + (across[1] * u + upward[1] * v) / R, n[2] + (across[2] * u + upward[2] * v) / R]);
      const r = R + lift;
      return P.point(c[0] + d[0] * r, c[1] + d[1] * r, c[2] + d[2] * r, col, al, s);
    };
    const lidAt = (/** @type {number} */ u) => h * open * Math.max(0, 1 - (u / w) ** 2);
    const rise = (/** @type {number} */ u) => h * 0.6 * Math.max(0, 1 - (u / w) ** 2);
    // The lids, raised most in the middle, and a fainter socket round them.
    for (const sign of [1, -1]) {
      const m = 12;
      const l0 = P.count;
      for (let i = 0; i <= m; i++) {
        const u = w * (-1 + (2 * i) / m);
        put(u + shake(o, e + i), sign * lidAt(u), rise(u) * (sign > 0 ? 1.15 : 0.9), o.col, a * 0.95, 1.0);
      }
      for (let i = 0; i < m; i++) P.link(l0 + i, l0 + i + 1, a * 0.75);
      const k0 = P.count;
      for (let i = 0; i <= m; i += 2) {
        const u = w * 1.18 * (-1 + (2 * i) / m);
        put(u, sign * (h * 1.25 * Math.max(0, 1 - (u / (w * 1.18)) ** 2) + h * 0.12), 0.02, o.col, a * 0.35, 0.8);
      }
      for (let i = 0; i < m / 2; i++) P.link(k0 + i, k0 + i + 1, a * 0.25);
      P.link(l0, k0, a * 0.25);
      P.link(l0 + m, k0 + m / 2, a * 0.25);
    }
    // The iris, domed out of the eye and turned towards what all the eyes
    // look at, with its pupil a short bright slit.
    const d = [o.gaze[0] - c[0] - n[0] * R, o.gaze[1] - c[1] - n[1] * R, o.gaze[2] - c[2] - n[2] * R];
    const dl = Math.hypot(d[0], d[1], d[2]) || 1;
    const gu = ((d[0] * across[0] + d[1] * across[1] + d[2] * across[2]) / dl) * w * 0.4;
    const gv = ((d[0] * upward[0] + d[1] * upward[1] + d[2] * upward[2]) / dl) * h * 0.4;
    const ri = h * 0.95;
    const i0 = P.count;
    const k = 12;
    for (let i = 0; i < k; i++) {
      const an = (i / k) * Math.PI * 2;
      const u = gu + Math.cos(an) * ri;
      const v = gv + Math.sin(an) * ri;
      put(u, v, h * 0.55, o.col, Math.abs(v) < lidAt(u) ? a * 0.9 : 0, 0.95);
    }
    for (let i = 0; i < k; i++) P.link(i0 + i, i0 + ((i + 1) % k), a * 0.5 * open);
    for (const v of [-0.5, -0.25, 0, 0.25, 0.5]) {
      if (Math.abs(gv + v * ri) < lidAt(gu)) put(gu, gv + v * ri, h * 0.75, o.hot, a * (1 + 0.6 * o.dilate), v === 0 ? 1.5 : 1.05);
    }
  }
  return nucleus;
}
