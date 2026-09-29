// The feed, and what the map's corner shows of it (#103).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { addNote, ageText, mapFeed, notesSince, placeKey, trim, FEED_SIZE } from '../web/js/feed.js';

const now = Date.parse('2026-09-28T12:00:00Z');
const at = (msAgo) => new Date(now - msAgo).toISOString();
const note = (id, msAgo, extra = {}) => ({ id, at: at(msAgo), place: { application: 'shop', environment: 'prod' }, loudness: 'quiet', message: `n${id}`, ...extra });

test('it keeps the last 200 notes or 24 hours of them, whichever is fewer', () => {
  let feed = [];
  for (let i = 0; i < 250; i++) feed = addNote(feed, note(i, 1000), now);
  assert.equal(feed.length, FEED_SIZE);
  assert.equal(feed[0].id, 50);
  assert.deepEqual(trim([note(1, 25 * 3600 * 1000), note(2, 1000)], now).map((n) => n.id), [2]);
});

test('the map shows the latest six, newest at the bottom, each with a dot and an age', () => {
  const feed = [
    note(1, 90000),
    note(2, 80000, { loudness: 'loud' }),
    note(3, 70000, { loudness: 'normal' }),
    note(4, 60000, { feedOnly: true }),
    note(5, 50000, { seeded: true }),
    note(6, 40000, { seam: true, place: {} }),
    note(7, 3000, { place: { component: 'traefik' } }),
  ];
  const shown = mapFeed(feed, now);
  assert.deepEqual(shown.map((e) => e.id), [2, 3, 4, 5, 6, 7]);
  assert.deepEqual(shown.map((e) => e.tone), ['loud', 'normal', 'resolved', 'resolved', 'seam', 'quiet']);
  assert.deepEqual(shown.map((e) => e.key), ['shop', 'shop', 'shop', 'shop', null, 'platform']);
  assert.deepEqual(shown.map((e) => e.age), ['1 min', '1 min', '1 min', '50 s', '40 s', 'now']);
});

test('places and ages', () => {
  assert.equal(placeKey({ application: 'shop', environment: 'prod' }), 'shop');
  assert.equal(placeKey({ component: 'argocd' }), 'platform');
  assert.equal(placeKey({}), 'platform');
  assert.deepEqual([0, 12000, 125000, 3 * 3600000, 2 * 86400000].map(ageText), ['now', '12 s', '2 min', '3 h', '2 d']);
});

test('a folded feed counts the notes since it was folded, by time, without the seam', () => {
  const feed = [note(7, 90000), note(8, 60000), note(1, 30000), note(2, 20000, { seam: true, place: {} }), note(3, 1000)];
  assert.deepEqual(notesSince(feed, at(60000)).map((n) => n.id), [1, 3]);
  assert.deepEqual(notesSince(feed, at(0)), []);
  assert.deepEqual(notesSince(feed, null).map((n) => n.id), [7, 8, 1, 3]);
});
