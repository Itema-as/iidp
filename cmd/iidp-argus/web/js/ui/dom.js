// @ts-check
// Building DOM without HTML strings, so text is always text. The page sets
// styles through element.style, never as a style attribute, so its
// Content-Security-Policy needs no 'unsafe-inline'.

/**
 * An element with attributes and children. Attributes named on* are
 * listeners; class is the class name; everything else is an attribute.
 * Strings among the children are text.
 * @param {string} tag
 * @param {Record<string, string | boolean | ((e: Event) => void) | undefined>} [attrs]
 * @param {(Node | string | null | undefined | false)[]} children
 * @returns {HTMLElement}
 */
export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === false) continue;
    if (typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className = String(v);
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children) if (c !== null && c !== undefined && c !== false) el.append(c);
  return el;
}

/**
 * Only http and https links are followed; anything else is shown as text.
 * @param {string} href
 */
export function safeHref(href) {
  try {
    const u = new URL(href, location.href);
    return u.protocol === 'https:' || u.protocol === 'http:' ? u.href : null;
  } catch {
    return null;
  }
}
