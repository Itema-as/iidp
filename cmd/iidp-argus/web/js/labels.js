// @ts-check
// Keeping labels from piling up where Applications crowd together (#115
// "Must be done in this build"). Each label is a box on the screen with a
// priority; the more important one keeps its place and the one it would
// cover is hidden until there is room again.

/**
 * @typedef {{id: string, x: number, y: number, w: number, h: number, priority: number}} Box
 *   x and y are the box's centre in screen pixels.
 */

/** Label priorities: a loud state's tag first, then what the pointer or card is on, then names. */
export const PRIORITY = {
  loudTag: 100,
  focused: 90,
  tag: 80,
  application: 70,
  component: 50,
  environment: 40,
  detail: 30,
};

/**
 * Which labels to show: greedily, by priority (then by id, so that the
 * choice is stable from frame to frame), each one that overlaps no label
 * already shown, with pad pixels between them. Boxes outside the view are
 * never shown.
 * @param {Box[]} boxes
 * @param {{width: number, height: number, pad?: number}} view
 * @returns {Set<string>}
 */
export function declutter(boxes, { width, height, pad = 3 }) {
  const order = [...boxes].sort((a, b) => b.priority - a.priority || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
  /** @type {Box[]} */
  const shown = [];
  const out = new Set();
  for (const b of order) {
    if (b.x + b.w / 2 < 0 || b.x - b.w / 2 > width || b.y + b.h / 2 < 0 || b.y - b.h / 2 > height) continue;
    const hit = shown.some((s) => Math.abs(s.x - b.x) * 2 < s.w + b.w + pad * 2 && Math.abs(s.y - b.y) * 2 < s.h + b.h + pad * 2);
    if (hit) continue;
    shown.push(b);
    out.add(b.id);
  }
  return out;
}
