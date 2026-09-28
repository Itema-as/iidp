// @ts-check
// The detail card on the page (#113, panel B): a peek next to what the
// pointer is on, which replaces tooltips; pinned by a click, with the
// read-only links and "All details"; kept next to its object as the
// camera moves; closed by Esc, its close button or a click on empty space.

import { h, safeHref } from './dom.js';
import { ageText } from '../feed.js';

/** @typedef {import('../card.js').Card} Card */

/**
 * @param {HTMLElement} el the card's element
 * @param {{onClose: () => void}} options
 */
export function createCardView(el, { onClose }) {
  let expanded = false;
  let last = '';
  let pinned = false;

  /** @param {Card} card @param {number} nowMs */
  function render(card, nowMs) {
    const chips = h('div', { class: 'chips' }, ...card.chips.map((c) => h('span', { class: `state ${c.tone}` }, c.text)));
    const head = [
      h('div', { class: 'kind' }, card.kind),
      h('h2', {}, card.title),
      pinned ? h('button', { class: 'close', type: 'button', 'aria-label': 'Close', onclick: onClose }, '×') : null,
      chips,
    ];
    const hops = card.hops
      ? [
          h('ol', { class: 'hops', 'aria-label': 'The Deploy\'s hops' }, ...card.hops.steps.map((s) => h('li', { class: s.state }, s.label))),
          h('div', { class: 'hopcap' }, card.hops.caption),
        ]
      : [];
    /** @param {[string, string][]} rows */
    const facts = (rows) => h('dl', {}, ...rows.flatMap(([k, v]) => [h('dt', {}, k), h('dd', {}, v)]));
    const capChips = card.capabilities.length
      ? h('div', { class: 'capchips' }, ...card.capabilities.map((c) => h('span', { class: `state ${c.tone}${c.outlined ? ' outlined' : ''}` }, c.name)))
      : null;

    /** @type {(Node | null)[]} */
    const body = [];
    if (!pinned || !expanded) body.push(facts(card.peek));
    body.push(capChips);
    if (pinned) {
      const links = card.links
        .map((l) => ({ ...l, href: safeHref(l.href) }))
        .map((l) => (l.href ? h('a', { href: l.href, target: '_blank', rel: 'noopener noreferrer', title: l.note }, l.text) : h('span', {}, l.text)));
      if (links.length) body.push(h('div', { class: 'links' }, ...links));
      body.push(h('button', { class: 'more', type: 'button', 'aria-expanded': String(expanded), onclick: () => {
        expanded = !expanded;
        last = '';
        render(card, nowMs);
      } }, expanded ? 'Fewer details' : 'All details'));
      if (expanded) {
        for (const s of card.sections) body.push(h('h3', {}, s.title), facts(s.rows));
        if (card.capabilities.length) {
          body.push(h('h3', {}, 'Capabilities'), h('ul', { class: 'caps' }, ...card.capabilities.map((c) =>
            h('li', { class: c.outlined ? 'outlined' : '' }, h('span', {}, c.name), ' ', h('span', { class: `state ${c.tone}` }, c.state), c.note ? h('span', { class: 'note' }, c.note) : null))));
        }
        if (card.recent.length) {
          body.push(h('h3', {}, 'Recent'), h('ul', { class: 'recent' }, ...card.recent.map((r) =>
            h('li', {}, h('span', { class: `dot ${r.tone}` }), h('time', { datetime: r.at }, ageText(nowMs - Date.parse(r.at))), h('span', {}, r.message)))));
        }
      }
    } else {
      body.push(h('div', { class: 'hint' }, 'Click to pin it and get the links'));
    }
    el.replaceChildren(...head.filter((x) => x !== null), ...hops, ...body.filter((x) => x !== null));
  }

  return {
    /**
     * Shows a card: a peek, or pinned. It is drawn again only when what
     * it says has changed, so that links under the pointer stay put.
     * @param {Card | null} card @param {boolean} isPinned @param {number} nowMs
     */
    show(card, isPinned, nowMs) {
      if (!card) {
        el.hidden = true;
        last = '';
        return;
      }
      if (isPinned !== pinned) {
        pinned = isPinned;
        if (!pinned) expanded = false;
        last = '';
      }
      const key = JSON.stringify(card) + Math.floor(nowMs / 30000);
      el.hidden = false;
      el.classList.toggle('pinned', pinned);
      el.classList.toggle('peek', !pinned);
      if (key === last) return;
      last = key;
      render(card, nowMs);
    },

    /** A new card: start collapsed. */
    reset() {
      expanded = false;
      last = '';
    },

    /**
     * Keeps the card next to its object, which is at x, y on the stage and
     * reaches r pixels from there, within the stage.
     * @param {number} x @param {number} y @param {number} r @param {DOMRect} stage
     */
    place(x, y, r, stage) {
      if (el.hidden) return;
      const w = el.offsetWidth;
      const hgt = el.offsetHeight;
      const gap = Math.min(Math.max(r, 12), 260) + 16;
      let left = stage.left + x + gap;
      if (left + w > stage.right - 8) left = stage.left + x - w - gap;
      left = Math.max(stage.left + 8, left);
      const top = Math.min(Math.max(stage.top + y - 40, stage.top + 60), stage.bottom - hgt - 12);
      el.style.transform = `translate(${Math.round(left)}px, ${Math.round(Math.max(stage.top + 8, top))}px)`;
    },
  };
}
