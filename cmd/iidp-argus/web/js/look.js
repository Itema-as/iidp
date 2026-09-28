// @ts-check
// How each object reads (#100, #115 "States"): which of the drawing's looks
// it takes, and the tag it wears. Loud looks have a shape as well as a
// colour, so that they can be told apart without colour: a stuck cloud
// freezes behind a hard ring, a Degraded one breaks its links apart, an
// Unknown one collapses to a dotted outline, and their tags carry a mark
// as well as a word.

/** @typedef {import('./types.js').Environment} Environment */
/** @typedef {import('./types.js').Component} Component */
/** @typedef {import('./types.js').Condition} Condition */
/** @typedef {import('./types.js').Activity} Activity */

/**
 * @typedef {'healthy' | 'working' | 'arriving' | 'unreleased' | 'leaving' | 'stuck' | 'degraded' | 'unknown'} Tone
 * @typedef {'none' | 'ring' | 'broken' | 'dotted' | 'plot'} Shape
 * @typedef {{text: string, tone: Tone | 'new' | 'warn'}} Tag
 * @typedef {{tone: Tone, shape: Shape, wave: boolean, tag: Tag | null}} Look
 */

/** The tags, each with a mark that is not a colour. */
export const TAGS = {
  stuck: { text: '■ STUCK', tone: /** @type {const} */ ('stuck') },
  degraded: { text: '✕ DEGRADED', tone: /** @type {const} */ ('degraded') },
  unknown: { text: '? UNKNOWN', tone: /** @type {const} */ ('unknown') },
  unreleased: { text: 'UNRELEASED', tone: /** @type {const} */ ('unreleased') },
  leaving: { text: 'LEAVING', tone: /** @type {const} */ ('leaving') },
  newApp: { text: '+ NEW · SETTING UP', tone: /** @type {const} */ ('new') },
  new: { text: '+ NEW', tone: /** @type {const} */ ('new') },
};

/**
 * An Environment's look. The order is the spec's: an Unreleased plot, then
 * Leaving, then stuck, then Degraded, then Arriving, then Unknown, then a
 * change under way. A lost cluster makes every Condition Unknown (#100);
 * the drawing greys the whole scene for it, so it is not repeated here.
 * @param {Environment} env
 * @param {{newApp?: boolean, preview?: boolean}} [context] newApp: the
 *   whole Application is arriving; preview: a Preview Environment, whose
 *   arrival is quiet and wears no tag.
 * @returns {Look}
 */
export function envLook(env, { newApp = false, preview = false } = {}) {
  const a = env.activity;
  const c = env.condition?.state ?? 'Healthy';
  const working = !!a && (a.state === 'Deploying' || a.state === 'Updating');
  if (a?.state === 'Unreleased') return { tone: 'unreleased', shape: 'plot', wave: false, tag: TAGS.unreleased };
  if (a?.state === 'Leaving') return { tone: 'leaving', shape: a.stuck ? 'ring' : 'none', wave: false, tag: a.stuck ? TAGS.stuck : TAGS.leaving };
  if (a?.stuck) return { tone: 'stuck', shape: 'ring', wave: false, tag: TAGS.stuck };
  if (c === 'Degraded') return { tone: 'degraded', shape: 'broken', wave: working, tag: TAGS.degraded };
  if (a?.state === 'Arriving') return { tone: 'arriving', shape: 'none', wave: false, tag: preview ? null : newApp ? TAGS.newApp : TAGS.new };
  if (c === 'Unknown') return { tone: 'unknown', shape: 'dotted', wave: false, tag: TAGS.unknown };
  if (working) return { tone: 'working', shape: 'none', wave: true, tag: null };
  return { tone: 'healthy', shape: 'none', wave: false, tag: null };
}

/**
 * A Platform component's look: a Condition, and Updating for one ArgoCD
 * manages.
 * @param {Component} c
 * @returns {Look}
 */
export function componentLook(c) {
  if (c.activity?.stuck) return { tone: 'stuck', shape: 'ring', wave: false, tag: TAGS.stuck };
  if (c.condition.state === 'Degraded') return { tone: 'degraded', shape: 'broken', wave: false, tag: TAGS.degraded };
  if (c.condition.state === 'Unknown') return { tone: 'unknown', shape: 'dotted', wave: false, tag: TAGS.unknown };
  if (c.activity) return { tone: 'working', shape: 'none', wave: true, tag: null };
  return { tone: 'healthy', shape: 'none', wave: false, tag: null };
}

/**
 * A Capability's warning tag, "! BACKUPS" and the like: the Warning flag
 * is a small flag, not Degraded.
 * @param {import('./types.js').Capability} cap
 * @returns {string | null}
 */
export function warningTag(cap) {
  const w = cap.condition.warning;
  if (!w) return null;
  if (cap.type === 'postgres') return /wal/i.test(w) ? '! ARCHIVING' : '! BACKUPS';
  if (cap.type === 'custom-domain') return '! CERTIFICATE';
  if (cap.type === 'scheduled-task') return '! TASK FAILED';
  return '! WARNING';
}

/**
 * A Condition and an Activity in words, for the card's chips.
 * @param {Condition | undefined} condition
 * @param {Activity | null} activity
 * @returns {{text: string, tone: Tone | 'new' | 'warn'}[]}
 */
export function stateChips(condition, activity) {
  /** @type {{text: string, tone: Tone | 'new' | 'warn'}[]} */
  const out = [];
  if (activity?.state !== 'Unreleased' && condition) {
    const tone = condition.state === 'Degraded' ? 'degraded' : condition.state === 'Unknown' ? 'unknown' : 'healthy';
    out.push({ text: condition.state, tone });
  }
  if (activity) {
    /** @type {Tone | 'new'} */
    let tone = 'working';
    if (activity.stuck) tone = 'stuck';
    else if (activity.state === 'Arriving') tone = 'new';
    else if (activity.state === 'Unreleased') tone = 'unreleased';
    else if (activity.state === 'Leaving') tone = 'leaving';
    out.push({ text: activity.stuck ? `${activity.state}, stuck` : activity.state, tone });
  }
  return out;
}
