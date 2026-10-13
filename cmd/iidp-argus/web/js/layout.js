// @ts-check
// Where things sit, as plain numbers: the Platform core (argocd) at the
// centre, the other components in groups by role round it with k3s beneath
// it as its floor, and each Application on an orbit slot of its own, joined
// to the core by a filament. Nothing here imports three.js; the drawing turns
// these into vectors.

/** @typedef {import('./types.js').Vec} Vec */

/** The radius of the ring the groups of Platform components sit on. */
export const COMPONENT_RING = 8.5;
/** The radius of the Platform core's cloud. */
export const CORE_RADIUS = 3;
/** The radius of a Platform component's cloud. */
export const COMPONENT_RADIUS = 0.7;
/** Where k3s, the node itself, sits: beneath the core, as its floor. */
export const FLOOR = /** @type {Vec} */ ([0, -6.5, 0]);

/**
 * The Platform components by role, in the order the groups take places on
 * the ring and their members places in their group, so that each keeps its
 * place whichever others the Platform has. argocd is the core itself and
 * k3s the floor. Any other component goes in a group of its own further
 * out.
 * @type {{key: string, title: string, members: string[]}[]}
 */
export const COMPONENT_GROUPS = [
  { key: 'edge', title: 'Edge', members: ['traefik', 'cert-manager', 'platform-tls', 'external-dns', 'oauth2-proxy'] },
  { key: 'delivery', title: 'Delivery', members: ['deploy-gate', 'guardrails'] },
  { key: 'data', title: 'Data', members: ['cloudnative-pg', 'cnpg-barman-cloud', 'db-tunnel'] },
  { key: 'watch', title: 'Watch', members: ['monitoring', 'argus'] },
];
/**
 * How far the groups are turned round the core: the camera's usual angle
 * looks along the diagonal between two of them, so that none sits right in
 * front of the core or behind it, under its labels.
 */
const GROUP_TURN = Math.PI / 2;
/** The group of the components no other group names. */
export const OTHER_GROUP = { key: 'other', title: 'Other' };

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
 * @typedef {{key: string, title: string, centre: Vec, members: string[]}} ComponentGroup
 *   members: the group's components the Platform has.
 */

/**
 * Where a group's members sit round its centre: on a small ring, each at
 * the place its index in the group gives it.
 * @param {Vec} centre @param {number} i @param {number} n
 * @returns {Vec}
 */
function memberPlace(centre, i, n) {
  if (n === 1) return centre;
  const r = 0.9 + 0.2 * n;
  const a = (i / n) * Math.PI * 2;
  return [centre[0] + Math.cos(a) * r, centre[1] + Math.sin(a * 2) * 0.35, centre[2] + Math.sin(a) * r];
}

/**
 * Where each Platform component sits, and the groups they sit in: argocd
 * is the core, k3s the floor, the known ones have fixed places in their
 * groups on the ring, and any other one a place in a group further out.
 * @param {string[]} names
 * @returns {{places: Map<string, Vec>, groups: ComponentGroup[]}}
 */
export function componentPlaces(names) {
  /** @type {Map<string, Vec>} */
  const places = new Map();
  /** @type {ComponentGroup[]} */
  const groups = [];
  if (names.includes('argocd')) places.set('argocd', [0, 0, 0]);
  if (names.includes('k3s')) places.set('k3s', FLOOR);
  COMPONENT_GROUPS.forEach((g, gi) => {
    const a = (gi / COMPONENT_GROUPS.length) * Math.PI * 2 + GROUP_TURN;
    /** @type {Vec} */
    const centre = [Math.cos(a) * COMPONENT_RING, 0, Math.sin(a) * COMPONENT_RING];
    const members = g.members.filter((n) => names.includes(n));
    if (!members.length) return;
    g.members.forEach((n, i) => {
      if (names.includes(n)) places.set(n, memberPlace(centre, i, g.members.length));
    });
    groups.push({ key: g.key, title: g.title, centre, members });
  });
  const others = names.filter((n) => !places.has(n)).sort();
  if (others.length) {
    // Between the first two groups, beyond the ring.
    const a = (0.5 / COMPONENT_GROUPS.length) * Math.PI * 2 + GROUP_TURN;
    /** @type {Vec} */
    const centre = [Math.cos(a) * (COMPONENT_RING + 4), 0, Math.sin(a) * (COMPONENT_RING + 4)];
    others.forEach((n, i) => places.set(n, memberPlace(centre, i, others.length)));
    groups.push({ ...OTHER_GROUP, centre, members: others });
  }
  return { places, groups };
}

/**
 * Where an Environment's Postgres marker sits: below its cloud.
 * @param {AppLayout} L @param {Vec} centre @param {number} R
 */
export function postgresPlace(L, centre, R) {
  return add(add(centre, scale(L.up, -(R + 0.8))), scale(L.radial, 0.7));
}
/**
 * Where an Environment's custom domain marker sits: above its cloud.
 * @param {AppLayout} L @param {Vec} centre @param {number} R
 */
export function domainPlace(L, centre, R) {
  return add(centre, scale(L.up, R + 1.0));
}

/**
 * How far out the whole Platform reaches, for the camera's wide view.
 * @param {number[]} slots
 */
export function platformReach(slots) {
  let r = COMPONENT_RING + 4;
  for (const s of slots) r = Math.max(r, orbitRadius(ringOf(s).k) + 5);
  return r;
}
