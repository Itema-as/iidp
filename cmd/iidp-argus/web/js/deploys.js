// @ts-check

/** @typedef {import('./types.js').Deploy} Deploy */
/** @typedef {import('./types.js').Environment} Environment */
/** @typedef {import('./types.js').Hop} Hop */

/** @type {Hop[]} */
export const HOPS = ['Accepted', 'WaitingForArgoCD', 'Applying', 'RollingOut', 'Serving'];

/** @type {Record<Hop, string>} */
export const HOP_LABEL = {
  Accepted: 'Accepted',
  WaitingForArgoCD: 'Waiting for ArgoCD',
  Applying: 'Applying',
  RollingOut: 'Rolling out',
  Serving: 'Serving',
};

/** @type {Record<Hop, string>} What a Deploy at each hop is doing, for a caption. */
const HOP_DOING = {
  Accepted: 'accepted by the Deploy gate',
  WaitingForArgoCD: 'waiting for ArgoCD',
  Applying: 'being applied',
  RollingOut: 'rolling out',
  Serving: 'serving',
};

/**
 * A Deploy's identity within its Environment: its time and tag, or its
 * tag alone when the Deploy gate's Event was not seen.
 * @param {Deploy} d
 */
export function deployKey(d) {
  return `${d.at ?? ''}|${d.tag}`;
}

/**
 * The Deploy under way in an Environment: the Activity's, for Deploying
 * and a first Deploy's Arriving.
 * @param {Environment} env
 * @returns {Deploy | null}
 */
export function currentDeploy(env) {
  return env.activity?.deploy ?? null;
}

/**
 * The hop strip: each of the five hops done, now, stuck or still to come,
 * and a caption. A Preview Environment's Deploy never passes the gate, so
 * its first two hops are not drawn as done but as skipped.
 * @param {Deploy} d
 * @returns {{steps: {hop: Hop, label: string, state: 'done' | 'now' | 'stuck' | 'todo' | 'skipped'}[], caption: string}}
 */
export function hopStrip(d) {
  const at = d.hop ? HOPS.indexOf(d.hop) : -1;
  const steps = HOPS.map((hop, i) => {
    /** @type {'done' | 'now' | 'stuck' | 'todo' | 'skipped'} */
    let state = i < at ? 'done' : i === at ? (d.stuck ? 'stuck' : d.hop === 'Serving' ? 'done' : 'now') : 'todo';
    if (d.preview && i < 2) state = 'skipped';
    return { hop, label: HOP_LABEL[hop], state };
  });
  const what = d.promote ? `Promote of ${d.tag}` : `Deploy of ${d.tag}`;
  let caption;
  if (d.refused) caption = `${what} refused: ${d.reason ?? ''}`;
  else if (d.supersededBy) caption = `${what} superseded by ${d.supersededBy}`;
  else if (!d.hop) caption = what;
  else if (d.stuck) caption = `${what} is stuck at ${HOP_LABEL[d.hop]}: ${d.reason ?? ''}`;
  else caption = `${what} is ${HOP_DOING[d.hop]}`;
  return { steps, caption };
}

/**
 * What the page remembers about a Deploy it has seen: when it first saw
 * it, whether it saw it happen (rather than find it in a snapshot), and
 * when it saw it reach its current hop, supersede or end.
 * @typedef {{first: number, live: boolean, hop: Hop | '', hopAt: number, supersededAt: number, lastHop: Hop | ''}} DeployLife
 */

/**
 * A waypoint of a bead's route: the Deploy gate's cluster, the core's
 * edge towards the Application, its hub, this Environment's cloud, or the
 * Application's staging cloud. A leg between the core's edge and the hub
 * follows the filament.
 * @typedef {'gate' | 'core' | 'coreEdge' | 'hub' | 'env' | 'staging'} Waypoint
 */

/**
 * @typedef {{
 *   route: Waypoint[], k: number,
 *   tone: 'deploy' | 'promote' | 'stuck' | 'refused' | 'backup',
 *   alpha: number, blink: boolean, pulse: boolean, flash: number, trail: boolean
 * }} Bead
 */

const clamp01 = (/** @type {number} */ q) => Math.min(1, Math.max(0, q));

/** How long a bead takes over each leg of its hop, in ms. */
export const TRAVEL = { gate: 1600, promoteLift: 2800, filament: 2500, enter: 1500, flash: 1200, fizzle: 2600, merge: 1500 };

/**
 * Where a hop holds its bead once it has arrived, and the route it took
 * there. A stuck Deploy freezes where it stopped.
 * @param {Hop} hop
 * @param {boolean} promote
 * @returns {{route: Waypoint[], travel: number}}
 */
function hopRoute(hop, promote) {
  switch (hop) {
    case 'Accepted':
      return promote ? { route: ['staging', 'hub', 'coreEdge'], travel: TRAVEL.promoteLift } : { route: ['gate', 'coreEdge'], travel: TRAVEL.gate };
    case 'WaitingForArgoCD':
      return { route: ['coreEdge'], travel: 0 };
    case 'Applying':
      return { route: ['coreEdge', 'hub'], travel: TRAVEL.filament };
    case 'RollingOut':
      return { route: ['hub', 'env'], travel: TRAVEL.enter };
    default:
      return { route: ['env'], travel: 0 };
  }
}

/**
 * The beads an Environment's Deploys draw now.
 * - The Deploy under way travels its hop's leg and holds at its end:
 *   Accepted from the gate (or, for a Promote, off the staging cloud and
 *   along the filament) into the core, Waiting for ArgoCD pulsing at the
 *   core's edge, Applying along the filament to the hub, and Rolling out
 *   into the cloud. Stuck, it freezes orange where it stopped, blinking.
 * - Serving flashes once, if the page saw it arrive.
 * - A refused Deploy's red bead goes from the gate to the core and fizzles
 *   out, if the page saw it refused.
 * - A superseded one dims and merges into the next.
 * - A Leaving Environment with a database sends its final backup home.
 * @param {Environment} env
 * @param {(d: Deploy) => DeployLife | undefined} lifeOf
 * @param {number} t the page's clock, as the lives record it
 * @returns {Bead[]}
 */
export function beadsFor(env, lifeOf, t) {
  /** @type {Bead[]} */
  const out = [];
  const bead = (/** @type {Partial<Bead> & {route: Waypoint[], tone: Bead['tone']}} */ b) =>
    out.push({ k: 1, alpha: 1, blink: false, pulse: false, flash: 0, trail: false, ...b });

  for (const d of env.deploys ?? []) {
    const life = lifeOf(d);
    if (!life) continue;
    if (d.refused) {
      const q = (t - life.first) / TRAVEL.fizzle;
      if (life.live && q < 1) bead({ route: ['gate', 'core'], tone: 'refused', k: clamp01(q * 1.6), alpha: q < 0.62 ? 1 : 1 - (q - 0.62) / 0.38, trail: true });
      continue;
    }
    if (d.supersededBy) {
      const q = (t - life.supersededAt) / TRAVEL.merge;
      if (life.live && life.supersededAt > 0 && q < 1 && life.lastHop) {
        const { route } = hopRoute(life.lastHop, !!d.promote);
        bead({ route: [route[route.length - 1], 'coreEdge'], tone: d.promote ? 'promote' : 'deploy', k: clamp01(q), alpha: 0.35 * (1 - q) });
      }
      continue;
    }
    if (!d.hop) continue;
    if (d.hop === 'Serving') {
      const q = (t - life.hopAt) / TRAVEL.flash;
      if (life.live && life.lastHop && life.lastHop !== 'Serving' && q < 1) bead({ route: ['env'], tone: d.promote ? 'promote' : 'deploy', flash: 1 - q });
      continue;
    }
    const { route, travel } = hopRoute(d.hop, !!d.promote);
    if (d.stuck) {
      // Frozen where it stopped: part-way into the cloud for a rollout,
      // otherwise at the end of its hop's leg.
      bead({ route, tone: 'stuck', k: d.hop === 'RollingOut' ? 0.55 : 1, blink: true });
      continue;
    }
    const k = life.live && travel > 0 ? clamp01((t - life.hopAt) / travel) : 1;
    bead({
      route, tone: d.promote ? 'promote' : 'deploy', k, trail: k < 1,
      pulse: d.hop === 'WaitingForArgoCD' || (d.hop === 'Applying' && k === 1),
      alpha: d.hop === 'RollingOut' ? 1 - 0.7 * k : 1,
    });
  }

  if (env.activity?.state === 'Leaving' && env.capabilities?.some((c) => c.type === 'postgres')) {
    for (let i = 0; i < 3; i++) bead({ route: ['hub', 'coreEdge'], tone: 'backup', k: (t / 1500 + i / 3) % 1 });
  }
  return out;
}
