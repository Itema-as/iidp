// @ts-check
// The drawing's seam (#97, #115): everything the rest of the page asks of
// the drawing. Build it in a container; draw a frame from the model; say
// where an Application or a thing is, what is under the pointer, and
// where a point is on the screen; and fly the camera where the automatic
// camera or a click says. A second drawing would implement the same.

import { createStage } from './stage.js';
import { createNetwork } from './network.js';

/** @typedef {import('../types.js').Vec} Vec */
/** @typedef {import('../types.js').Target} Target */
/** @typedef {import('../model.js').Model} Model */
/** @typedef {import('./network.js').View} View */

/** How close the camera comes, by what it looks at. */
const DISTANCE = { app: 30, platform: 34, core: 34, component: 16, env: 18 };

/**
 * @param {HTMLElement} container
 * @param {{reduced: boolean, onInput: (t: number) => void}} options
 */
export function createDrawing(container, options) {
  const stage = createStage(container, options);
  const network = createNetwork(stage, options);
  /** @type {Model | null} */
  let model = null;
  const homeDistance = () => stage.rig.distanceFor((model ? network.reach(model) : 20) + 4);

  return {
    /**
     * Draws a frame.
     * @param {number} t @param {View} view
     */
    frame(t, view) {
      model = view.model;
      stage.rig.update(t);
      network.frame(t, view);
      stage.render();
    },

    where: network.where,
    placeOf: network.placeOf,
    screenRadius: network.screenRadius,
    pick: network.pick,
    project: stage.project,
    stats: network.stats,
    lowerResolution: stage.lowerResolution,
    get pixelRatio() {
      return stage.pixelRatio;
    },
    get width() {
      return stage.width;
    },
    get height() {
      return stage.height;
    },

    camera: {
      /** Frames the whole Platform at once, without flying. @param {Model} m */
      start(m) {
        model = m;
        stage.rig.frameHome(homeDistance());
      },

      /** Back out over the whole Platform, circling slowly. @param {number} [speed] */
      home(speed = 0.22) {
        stage.rig.fly([0, 0, 0], homeDistance());
        stage.rig.circle(speed);
      },

      /**
       * Flies to an Application (or the Platform), keeps it centred, and
       * circles it. False when it is no longer on the map.
       * @param {string} key
       */
      visit(key) {
        const p = network.where(key);
        if (!p) return false;
        stage.rig.fly(p, key === 'platform' ? DISTANCE.platform : DISTANCE.app, () => network.where(key));
        stage.rig.circle(0.45);
        return true;
      },

      /**
       * One wide shot framing several places.
       * @param {string[]} keys
       */
      wide(keys) {
        const points = keys.map((k) => network.where(k)).filter((p) => p !== null);
        if (!points.length) return;
        const lo = [Infinity, Infinity, Infinity];
        const hi = [-Infinity, -Infinity, -Infinity];
        for (const p of points) for (let i = 0; i < 3; i++) {
          lo[i] = Math.min(lo[i], p[i]);
          hi[i] = Math.max(hi[i], p[i]);
        }
        /** @type {Vec} */
        const centre = [(lo[0] + hi[0]) / 2, (lo[1] + hi[1]) / 2, (lo[2] + hi[2]) / 2];
        const extent = Math.max(hi[0] - lo[0], hi[2] - lo[2]) / 2 + 14;
        stage.rig.fly(centre, Math.max(45, stage.rig.distanceFor(extent)));
        stage.rig.circle(0.3);
      },

      /** Holds still where it is, once a flight under way has landed. */
      hold() {
        stage.rig.hold();
      },

      /**
       * Flies to a place someone chose (a feed entry, an edge marker) and
       * holds there.
       * @param {string} key
       */
      go(key) {
        const p = network.where(key);
        if (!p) return;
        stage.rig.fly(p, key === 'platform' ? DISTANCE.platform : DISTANCE.app);
        stage.rig.circle(0);
      },

      /**
       * Flies to a thing whose card was pinned, and holds there.
       * @param {Target} target
       */
      show(target) {
        const p = network.placeOf(target);
        if (!p) return;
        stage.rig.fly(p, target.kind === 'env' ? DISTANCE.env : target.kind === 'component' ? DISTANCE.component : DISTANCE.core);
        stage.rig.circle(0);
      },
    },
  };
}

/** @typedef {ReturnType<typeof createDrawing>} Drawing */
