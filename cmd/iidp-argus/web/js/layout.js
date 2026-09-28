// @ts-check
// Where things sit (#102, #115 "The drawing"), as plain numbers: the
// Platform core at the centre with its components on a ring round it, and
// each Application on a fixed orbit slot with one filament to the core.
// Nothing here imports three.js; the drawing turns these into vectors.

/** @typedef {import('./types.js').Vec} Vec */

/** The radius of the ring the Platform components sit on. */
export const COMPONENT_RING = 8.5;
/** The radius of the Platform core's cloud. */
export const CORE_RADIUS = 3;

/**
 * The components in the order they take places on the ring, so that each
 * keeps its place whichever others the Platform has. argocd is the core
 * itself. Any other component takes a place on an outer ring, by name.
 */
export const COMPONENT_ORDER = [
  'traefik', 'deploy-gate', 'cert-manager', 'cloudnative-pg', 'cnpg-barman-cloud', 'external-dns',
  'oauth2-proxy', 'monitoring', 'guardrails', 'platform-tls', 'argus', 'k3s',
];

/**
 * An orbit slot's ring and angle: ring k holds 6k Applications, and every
 * other ring is offset half a step.
 * @param {number} slot 1, 2, 3, ...
 * @returns {{k: number, angle: number}}
 */
export function ringOf(slot) {
  let s = slot - 1;
  let k = 1;
  while (s >= 6 * k) {
    s -= 6 * k;
    k++;
  }
  const n = 6 * k;
  return { k, angle: (s / n) * Math.PI * 2 + (k % 2 === 0 ? Math.PI / n : 0) + Math.PI / 4 };
}

/** @param {number} k */
export function orbitRadius(k) {
  return 18 + (k - 1) * 14;
}

/** @param {Vec} a @param {Vec} b */
export const add = (a, b) => /** @type {Vec} */ ([a[0] + b[0], a[1] + b[1], a[2] + b[2]]);
/** @param {Vec} a @param {number} s */
export const scale = (a, s) => /** @type {Vec} */ ([a[0] * s, a[1] * s, a[2] * s]);
/** @param {Vec} a @param {Vec} b @param {number} k */
export const lerp = (a, b, k) => /** @type {Vec} */ ([a[0] + (b[0] - a[0]) * k, a[1] + (b[1] - a[1]) * k, a[2] + (b[2] - a[2]) * k]);
/** @param {Vec} a @param {Vec} b */
export const distance = (a, b) => Math.hypot(a[0] - b[0], a[1] - b[1], a[2] - b[2]);

/**
 * A point on a quadratic Bézier curve.
 * @param {Vec} a @param {Vec} c @param {Vec} b @param {number} k 0 at a, 1 at b
 * @returns {Vec}
 */
export function bezier(a, c, b, k) {
  const u = 1 - k;
  return [
    u * u * a[0] + 2 * u * k * c[0] + k * k * b[0],
    u * u * a[1] + 2 * u * k * c[1] + k * k * b[1],
    u * u * a[2] + 2 * u * k * c[2] + k * k * b[2],
  ];
}

/** The size of an Environment's cloud by its kind: its radius and particle count. */
export const CLOUD = {
  prod: { R: 1.9, n: 40 },
  staging: { R: 1.5, n: 28 },
  preview: { R: 0.95, n: 14 },
};

/** @param {string} env */
export function envKind(env) {
  return env === 'prod' ? 'prod' : env === 'staging' ? 'staging' : 'preview';
}

/** How many Preview Environments are drawn; the rest are counted as "+n". */
export const PREVIEWS_DRAWN = 3;

/**
 * @typedef {{
 *   centre: Vec, radial: Vec, tangent: Vec, up: Vec,
 *   filament: {start: Vec, control: Vec, end: Vec},
 *   envs: {name: string, kind: 'prod' | 'staging' | 'preview', centre: Vec, R: number, n: number}[],
 *   more: number
 * }} AppLayout
 */

/**
 * Where an Application in slot draws: its hub on its orbit, its filament
 * to the core's edge, and its Environments round the hub: prod and
 * staging, then up to three Preview Environments, with the rest counted.
 * @param {number} slot
 * @param {string[]} envNames in the stream's order: prod, staging, previews
 * @returns {AppLayout}
 */
export function appLayout(slot, envNames) {
  const { k, angle } = ringOf(slot);
  const r = orbitRadius(k);
  /** @type {Vec} */
  const centre = [Math.cos(angle) * r, Math.sin(angle * 2 + k) * 2.5, Math.sin(angle) * r];
  const len = Math.hypot(centre[0], centre[2]);
  /** @type {Vec} */
  const radial = [centre[0] / len, 0, centre[2] / len];
  /** @type {Vec} */
  const tangent = [-radial[2], 0, radial[0]];
  /** @type {Vec} */
  const up = [0, 1, 0];
  /** @param {number} t @param {number} u @param {number} q */
  const at = (t, u, q) => add(add(add(centre, scale(tangent, t)), scale(up, u)), scale(radial, q));

  const envs = [];
  const previews = envNames.filter((n) => envKind(n) === 'preview');
  if (envNames.includes('prod')) envs.push({ name: 'prod', kind: /** @type {const} */ ('prod'), centre: at(-2.7, 0.9, -0.3), ...CLOUD.prod });
  if (envNames.includes('staging')) envs.push({ name: 'staging', kind: /** @type {const} */ ('staging'), centre: at(2.7, -0.2, 0.2), ...CLOUD.staging });
  const drawn = previews.slice(0, PREVIEWS_DRAWN);
  drawn.forEach((name, i) => {
    envs.push({ name, kind: /** @type {const} */ ('preview'), centre: at((i - (drawn.length - 1) / 2) * 2.3, -1.9, 3.1), ...CLOUD.preview });
  });

  const start = /** @type {Vec} */ ([radial[0] * (CORE_RADIUS + 1), 0.3, radial[2] * (CORE_RADIUS + 1)]);
  const control = add(lerp(start, centre, 0.5), [0, 5, 0]);
  return { centre, radial, tangent, up, filament: { start, control, end: centre }, envs, more: previews.length - drawn.length };
}

/**
 * Where each Platform component sits: argocd is the core, the known ones
 * have fixed places on the ring, and any other one a place further out.
 * @param {string[]} names
 * @returns {Map<string, Vec>}
 */
export function componentPlaces(names) {
  /** @type {Map<string, Vec>} */
  const out = new Map();
  const others = names.filter((n) => n !== 'argocd' && !COMPONENT_ORDER.includes(n)).sort();
  for (const name of names) {
    if (name === 'argocd') {
      out.set(name, [0, 0, 0]);
      continue;
    }
    const i = COMPONENT_ORDER.indexOf(name);
    const angle = i >= 0 ? (i / COMPONENT_ORDER.length) * Math.PI * 2 + 0.3 : (others.indexOf(name) / Math.max(1, others.length)) * Math.PI * 2;
    const r = i >= 0 ? COMPONENT_RING : COMPONENT_RING + 3;
    out.set(name, [Math.cos(angle) * r, 0, Math.sin(angle) * r]);
  }
  return out;
}

/**
 * Where a Postgres cluster, a custom domain's marker sit on an
 * Environment's cloud.
 * @param {AppLayout} L @param {Vec} centre @param {number} R
 */
export function postgresPlace(L, centre, R) {
  return add(add(centre, scale(L.up, -(R + 0.8))), scale(L.radial, 0.7));
}
/** @param {AppLayout} L @param {Vec} centre @param {number} R */
export function domainPlace(L, centre, R) {
  return add(centre, scale(L.up, R + 1.0));
}

/**
 * How far out the whole Platform reaches, for the camera's wide view.
 * @param {number[]} slots
 */
export function platformReach(slots) {
  let r = COMPONENT_RING + 2;
  for (const s of slots) r = Math.max(r, orbitRadius(ringOf(s).k) + 5);
  return r;
}
