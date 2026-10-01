// @ts-check

/** @typedef {import('./types.js').Note} Note */
/** @typedef {import('./types.js').Place} Place */

/** The feed keeps the last FEED_SIZE notes or FEED_AGE of them, as the server does. */
export const FEED_SIZE = 200;
export const FEED_AGE = 24 * 60 * 60 * 1000;
/** The map shows the latest few. */
export const MAP_FEED = 6;

/**
 * The feed with note added, and trimmed.
 * @param {Note[]} feed oldest first
 * @param {Note} note
 * @param {number} nowMs wall-clock time
 * @returns {Note[]}
 */
export function addNote(feed, note, nowMs) {
  return trim([...feed, note], nowMs);
}

/**
 * @param {Note[]} feed
 * @param {number} nowMs
 * @returns {Note[]}
 */
export function trim(feed, nowMs) {
  const kept = feed.filter((n) => nowMs - Date.parse(n.at) <= FEED_AGE);
  return kept.slice(-FEED_SIZE);
}

/**
 * The key the camera's queue files a place under: its Application, or the
 * Platform for a component or the Platform as a whole.
 * @param {Place} place
 */
export function placeKey(place) {
  return place.application || 'platform';
}

/**
 * How long ago, briefly: "now", "12 s", "4 min", "3 h", "2 d".
 * @param {number} ms
 */
export function ageText(ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 5) return 'now';
  if (s < 60) return `${s} s`;
  if (s < 3600) return `${Math.floor(s / 60)} min`;
  if (s < 86400) return `${Math.floor(s / 3600)} h`;
  return `${Math.floor(s / 86400)} d`;
}

/**
 * What the dot says: loud red, normal cyan, quiet amber, and grey for a
 * note that only reports something finished or from before Argus started.
 * The seam gets its own mark.
 * @param {Note} n
 * @returns {'loud' | 'normal' | 'quiet' | 'resolved' | 'seam'}
 */
export function toneOf(n) {
  if (n.seam) return 'seam';
  if (n.feedOnly || n.seeded) return 'resolved';
  return n.loudness;
}

/**
 * @typedef {{id: number, tone: ReturnType<typeof toneOf>, message: string, age: string, key: string | null, place: Place}} FeedEntry
 */

/**
 * The map's feed: the latest MAP_FEED notes, newest at the bottom, each
 * with its dot, its age, and the place a click flies to (none for the
 * seam).
 * @param {Note[]} feed oldest first
 * @param {number} nowMs
 * @returns {FeedEntry[]}
 */
export function mapFeed(feed, nowMs) {
  return feed.slice(-MAP_FEED).map((n) => ({
    id: n.id,
    tone: toneOf(n),
    message: n.message,
    age: ageText(nowMs - Date.parse(n.at)),
    key: n.seam ? null : placeKey(n.place),
    place: n.place,
  }));
}

/**
 * The notes that came after since (a note's time), not counting the seam:
 * what a folded feed has not shown. By time, not id, because ids start
 * again when Argus does.
 * @param {Note[]} feed oldest first
 * @param {string | null} since the newest note's time when the feed was
 *   folded, or null if it was empty then
 * @returns {Note[]}
 */
export function notesSince(feed, since) {
  const after = since ? Date.parse(since) : -Infinity;
  return feed.filter((n) => !n.seam && Date.parse(n.at) > after);
}

/**
 * The latest notes about somewhere, newest first, for a card.
 * @param {Note[]} feed oldest first
 * @param {(p: Place) => boolean} about
 * @param {number} count
 * @returns {Note[]}
 */
export function notesAbout(feed, about, count) {
  const out = [];
  for (let i = feed.length - 1; i >= 0 && out.length < count; i--) {
    if (!feed[i].seam && about(feed[i].place)) out.push(feed[i]);
  }
  return out;
}
