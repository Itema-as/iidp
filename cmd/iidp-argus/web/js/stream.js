// @ts-check
// The page's connection to Argus: the server-sent events at /events
// (internal/argus/doc.go), and what to do when they stop.
//
// EventSource reconnects by itself after a dropped connection. It gives up
// for good (readyState CLOSED) when an answer is not a stream: after the
// Itema login session expires, oauth2-proxy answers with a redirect to
// the sign-in page, which EventSource cannot follow. Only loading the page
// again signs the person in again, so the page reloads itself then, and
// also when the stream has kept failing for a long time while Argus itself
// answers. While Argus does not answer at all, the page keeps the last
// known state on screen and keeps trying. Reloads are bounded
// (reloadAllowed), so a page that cannot recover never reloads in a fast
// loop.

/** The events the stream sends. */
export const EVENTS = ['snapshot', 'application', 'application-removed', 'component', 'component-removed', 'note', 'cluster'];

/** How long the stream may keep failing before the page reloads, if Argus answers. */
export const FAILING_FOR = 2 * 60 * 1000;
/** How often a failing stream asks Argus whether it answers. */
export const PROBE_EVERY = 30 * 1000;
/** Reloads are spaced at least this far apart, doubling with each recent one. */
export const RELOAD_GAP = 60 * 1000;
/** At most this many reloads in RELOAD_WINDOW. */
export const RELOAD_MAX = 5;
export const RELOAD_WINDOW = 30 * 60 * 1000;
/** Where the page keeps its recent reloads. */
export const RELOADS_KEY = 'argus.reloads';

/**
 * Whether the page may reload now, given when it last reloaded itself:
 * not more than RELOAD_MAX times in RELOAD_WINDOW, and never sooner than
 * RELOAD_GAP after the last reload, a gap that doubles with each recent
 * one (1, 2, 4, 8 minutes).
 * @param {number[]} reloads wall-clock times of recent reloads
 * @param {number} nowMs
 */
export function reloadAllowed(reloads, nowMs) {
  const recent = reloads.filter((r) => nowMs - r < RELOAD_WINDOW).sort((a, b) => a - b);
  if (recent.length >= RELOAD_MAX) return false;
  if (recent.length === 0) return true;
  const gap = RELOAD_GAP * 2 ** (recent.length - 1);
  return nowMs - recent[recent.length - 1] >= gap;
}

/**
 * The delay before trying the stream again by hand, after EventSource gave
 * up: 3 s, doubling, at most a minute.
 * @param {number} attempt 0, 1, 2, ...
 */
export function retryDelay(attempt) {
  return Math.min(60000, 3000 * 2 ** attempt);
}

/**
 * Whether the answer to a request Argus serves says the Itema login
 * session has expired: oauth2-proxy's redirect to the sign-in page, which
 * fetch with redirect "manual" reports as an opaque redirect, or its 401
 * to a request that wants no page.
 * @param {{type: string, status: number}} response
 */
export function loginExpired(response) {
  return response.type === 'opaqueredirect' || response.status === 401;
}

/**
 * @typedef {{
 *   url: string,
 *   onMessage: (type: string, data: any) => void,
 *   onState: (state: 'connecting' | 'open' | 'down') => void,
 *   EventSource?: typeof EventSource,
 *   fetch?: typeof fetch,
 *   storage?: Pick<Storage, 'getItem' | 'setItem'>,
 *   reload?: () => void,
 *   now?: () => number,
 *   log?: (message: string) => void,
 * }} StreamOptions
 */

/**
 * Connects to the stream and keeps connected.
 * @param {StreamOptions} options
 * @returns {{close: () => void}}
 */
export function connect(options) {
  const ES = options.EventSource ?? globalThis.EventSource;
  const doFetch = options.fetch ?? globalThis.fetch.bind(globalThis);
  const storage = options.storage ?? globalThis.sessionStorage;
  const reload = options.reload ?? (() => globalThis.location.reload());
  const now = options.now ?? Date.now;
  const log = options.log ?? ((m) => console.info(`Argus: ${m}`));

  /** @type {EventSource | null} */
  let source = null;
  let failingSince = 0;
  let attempt = 0;
  /** @type {ReturnType<typeof setTimeout> | undefined} */
  let timer;
  let closed = false;
  let lastProbe = -Infinity;

  const reloads = () => {
    try {
      return /** @type {number[]} */ (JSON.parse(storage.getItem(RELOADS_KEY) ?? '[]')).filter((n) => typeof n === 'number');
    } catch {
      return [];
    }
  };

  /** @param {string} why */
  const reloadIfAllowed = (why) => {
    const history = reloads();
    if (!reloadAllowed(history, now())) {
      log(`${why}; not reloading again yet`);
      return false;
    }
    storage.setItem(RELOADS_KEY, JSON.stringify([...history, now()].slice(-RELOAD_MAX)));
    log(`${why}; reloading the page`);
    reload();
    return true;
  };

  const open = () => {
    if (closed) return;
    options.onState('connecting');
    const es = new ES(options.url);
    source = es;
    es.onopen = () => options.onState('open');
    for (const type of EVENTS) {
      es.addEventListener(type, (/** @type {MessageEvent} */ e) => {
        if (type === 'snapshot') {
          failingSince = 0;
          attempt = 0;
        }
        let data;
        try {
          data = JSON.parse(e.data);
        } catch {
          log(`a ${type} message that is not JSON was dropped`);
          return;
        }
        options.onMessage(type, data);
      });
    }
    es.onerror = () => {
      options.onState('down');
      if (!failingSince) failingSince = now();
      const gaveUp = es.readyState === ES.CLOSED;
      const tooLong = now() - failingSince > FAILING_FOR;
      if (gaveUp) es.close();
      else if (!tooLong || now() - lastProbe < PROBE_EVERY) return; // EventSource retries by itself
      lastProbe = now();
      // Ask Argus directly: has the Itema login session expired, or is
      // Argus answering while its stream is not?
      doFetch('healthz', { redirect: 'manual', cache: 'no-store', credentials: 'same-origin' })
        .then((r) => {
          if (loginExpired(r)) return reloadIfAllowed('the Itema login session has expired');
          if (r.ok && tooLong) return reloadIfAllowed('the stream has kept failing while Argus answers');
          return false;
        })
        // Argus does not answer at all: keep showing the last known
        // state, and keep trying.
        .catch(() => false)
        .then((reloading) => {
          if (reloading || closed || !gaveUp) return;
          timer = setTimeout(open, retryDelay(attempt++));
        });
    };
  };

  open();
  return {
    close() {
      closed = true;
      clearTimeout(timer);
      source?.close();
    },
  };
}
