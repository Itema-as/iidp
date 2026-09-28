// @ts-check
// The stage: the renderer with its glow, the HTML labels' renderer, the
// perspective camera with OrbitControls (left-drag rotates, right-drag
// pans, scroll zooms), and the camera rig the automatic camera flies.

import * as THREE from 'three';
import { OrbitControls } from 'three/addons/controls/OrbitControls.js';
import { EffectComposer } from 'three/addons/postprocessing/EffectComposer.js';
import { RenderPass } from 'three/addons/postprocessing/RenderPass.js';
import { UnrealBloomPass } from 'three/addons/postprocessing/UnrealBloomPass.js';
import { OutputPass } from 'three/addons/postprocessing/OutputPass.js';
import { CSS2DRenderer } from 'three/addons/renderers/CSS2DRenderer.js';

/** @typedef {import('../types.js').Vec} Vec */

/** The camera's usual angle onto the Platform. */
const HOME_DIRECTION = new THREE.Vector3(0.6, 0.5, 0.62).normalize();
/** How long a flight takes, in ms. */
const FLIGHT = 1600;

const ease = (/** @type {number} */ q) => (q < 0.5 ? 4 * q * q * q : 1 - Math.pow(-2 * q + 2, 3) / 2);

/**
 * @param {HTMLElement} container
 * @param {{reduced: boolean, onInput: (t: number) => void}} options
 */
export function createStage(container, { reduced, onInput }) {
  const renderer = new THREE.WebGLRenderer({ antialias: true, powerPreference: 'high-performance' });
  let pixelRatio = Math.min(globalThis.devicePixelRatio || 1, 2);
  renderer.setPixelRatio(pixelRatio);
  container.appendChild(renderer.domElement);
  const labels = new CSS2DRenderer();
  labels.domElement.className = 'labels';
  container.appendChild(labels.domElement);

  const scene = new THREE.Scene();
  scene.background = new THREE.Color('#03050a');
  const camera = new THREE.PerspectiveCamera(42, 1, 0.1, 4000);
  const controls = new OrbitControls(camera, renderer.domElement);
  controls.enableDamping = true;
  controls.dampingFactor = 0.08;
  controls.mouseButtons = { LEFT: THREE.MOUSE.ROTATE, MIDDLE: THREE.MOUSE.DOLLY, RIGHT: THREE.MOUSE.PAN };
  controls.rotateSpeed = 0.6;
  controls.zoomSpeed = 1.2;
  controls.minDistance = 5;
  controls.maxDistance = 420;

  const composer = new EffectComposer(renderer);
  composer.addPass(new RenderPass(scene, camera));
  const bloom = new UnrealBloomPass(new THREE.Vector2(256, 256), 0.8, 0.45, 0.12);
  composer.addPass(bloom);
  composer.addPass(new OutputPass());

  /** Point sizes are scaled by this: pixels per world unit at distance 1, over 10. */
  const pointScale = { value: 200 };

  function resize() {
    const w = container.clientWidth || 1;
    const h = container.clientHeight || 1;
    renderer.setPixelRatio(pixelRatio);
    renderer.setSize(w, h, false);
    composer.setPixelRatio(pixelRatio);
    composer.setSize(w, h);
    labels.setSize(w, h);
    camera.aspect = w / h;
    camera.updateProjectionMatrix();
    pointScale.value = ((h * pixelRatio) / (2 * Math.tan(THREE.MathUtils.degToRad(camera.fov / 2)))) * 0.1;
  }
  new ResizeObserver(resize).observe(container);
  resize();

  // Someone using the map: dragging (past a few pixels, so that a click
  // is not), rotating, panning or scrolling. Nothing else counts.
  /** @type {{x: number, y: number} | null} */
  let down = null;
  renderer.domElement.addEventListener('pointerdown', (e) => {
    down = { x: e.clientX, y: e.clientY };
  });
  renderer.domElement.addEventListener('pointermove', (e) => {
    if (down && e.buttons && Math.hypot(e.clientX - down.x, e.clientY - down.y) > 4) {
      rig.stop();
      onInput(performance.now());
    }
  });
  addEventListener('pointerup', () => {
    down = null;
  });
  renderer.domElement.addEventListener('wheel', () => {
    rig.stop();
    onInput(performance.now());
  }, { passive: true });
  renderer.domElement.addEventListener('contextmenu', (e) => e.preventDefault());

  /**
   * @typedef {{from: THREE.Vector3, to: THREE.Vector3, dir: THREE.Vector3, d0: number, d1: number, t0: number, follow: (() => Vec | null) | null}} Flight
   */
  /** @type {Flight | null} */
  let flight = null;
  /** @type {(() => Vec | null) | null} */
  let following = null;
  const v = new THREE.Vector3();

  const rig = {
    /**
     * Flies to point at distance, keeping the current angle; follow, when
     * given, is where the point is now, for something that moves.
     * @param {Vec} point @param {number} distance @param {(() => Vec | null) | null} [follow]
     */
    fly(point, distance, follow = null) {
      const off = camera.position.clone().sub(controls.target);
      flight = { from: controls.target.clone(), to: new THREE.Vector3(...point), dir: off.clone().normalize(), d0: off.length(), d1: distance, t0: performance.now(), follow };
      following = follow;
    },

    /**
     * Circles slowly, degrees a second being speed × 6.
     * @param {number} speed
     */
    circle(speed) {
      controls.autoRotate = speed > 0;
      controls.autoRotateSpeed = reduced ? Math.min(speed, 0.1) : speed;
    },

    /** Stops where it is, at once: someone took the controls. */
    stop() {
      flight = null;
      following = null;
      controls.autoRotate = false;
    },

    /**
     * Stops circling and following, and lets a flight under way land:
     * one to a card someone pinned, say.
     */
    hold() {
      following = null;
      if (flight) flight.follow = null;
      controls.autoRotate = false;
    },

    /** The distance that shows everything within radius of the centre. @param {number} radius */
    distanceFor(radius) {
      return (radius / Math.tan(THREE.MathUtils.degToRad(camera.fov / 2))) * 0.68;
    },

    /** Points the camera straight at the Platform from its usual angle. @param {number} distance */
    frameHome(distance) {
      controls.target.set(0, 0, 0);
      camera.position.copy(HOME_DIRECTION).multiplyScalar(distance);
      controls.update();
    },

    /** Moves the flight on, and keeps a followed point centred. @param {number} t */
    update(t) {
      if (flight) {
        const q = reduced ? 1 : Math.min(1, (t - flight.t0) / FLIGHT);
        const e = ease(q);
        const to = flight.follow?.();
        if (to) flight.to.set(...to);
        controls.target.lerpVectors(flight.from, flight.to, e);
        camera.position.copy(controls.target).addScaledVector(flight.dir, THREE.MathUtils.lerp(flight.d0, flight.d1, e));
        if (q >= 1) flight = null;
      } else if (following) {
        const p = following();
        if (p) {
          v.set(...p).sub(controls.target);
          controls.target.add(v);
          camera.position.add(v);
        }
      }
      controls.update();
    },
  };

  const vp = new THREE.Vector3();
  return {
    renderer,
    scene,
    camera,
    controls,
    rig,
    pointScale,
    labelsElement: labels.domElement,

    /** The camera's right and up, for things drawn facing it. */
    basis() {
      return {
        right: /** @type {Vec} */ ([camera.matrixWorld.elements[0], camera.matrixWorld.elements[1], camera.matrixWorld.elements[2]]),
        up: /** @type {Vec} */ ([camera.matrixWorld.elements[4], camera.matrixWorld.elements[5], camera.matrixWorld.elements[6]]),
      };
    },

    /**
     * Where a point is on the screen, in CSS pixels from the stage's top
     * left, and whether it is in front of the camera.
     * @param {Vec} p
     */
    project(p) {
      vp.set(p[0], p[1], p[2]).project(camera);
      const w = container.clientWidth;
      const h = container.clientHeight;
      return { x: ((vp.x + 1) / 2) * w, y: ((1 - vp.y) / 2) * h, ndcX: vp.x, ndcY: vp.y, front: vp.z < 1 };
    },

    /** How many CSS pixels a world length at p spans. @param {Vec} p @param {number} length */
    pixels(p, length) {
      const d = camera.position.distanceTo(vp.set(p[0], p[1], p[2]));
      return (length * container.clientHeight) / (2 * Math.tan(THREE.MathUtils.degToRad(camera.fov / 2)) * Math.max(d, 0.001));
    },

    /** The camera's distance to p. @param {Vec} p */
    distanceTo(p) {
      return camera.position.distanceTo(vp.set(p[0], p[1], p[2]));
    },

    get width() {
      return container.clientWidth;
    },
    get height() {
      return container.clientHeight;
    },

    /** Renders the scene with its glow, then the labels. */
    render() {
      composer.render();
      labels.render(scene, camera);
    },

    /** Lowers the pixel ratio, for a GPU that cannot keep up. */
    lowerResolution() {
      if (pixelRatio <= 1) return false;
      pixelRatio = 1;
      resize();
      return true;
    },

    get pixelRatio() {
      return pixelRatio;
    },

    /** Whether the glow is on. @param {boolean} on */
    setGlow(on) {
      bloom.enabled = on;
    },
  };
}

/** @typedef {ReturnType<typeof createStage>} Stage */
