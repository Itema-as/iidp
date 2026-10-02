// @ts-check
// The particle network's buffers. They are refilled from scratch every frame,
// so a change of state never needs a rebuild.

import * as THREE from 'three';

/**
 * The point size is in tenths of a world unit, scaled by uScale, so that
 * particles shrink with distance like everything else.
 * @param {{value: number}} scale
 */
function pointMaterial(scale) {
  return new THREE.ShaderMaterial({
    uniforms: { uScale: scale },
    transparent: true,
    depthWrite: false,
    blending: THREE.AdditiveBlending,
    vertexShader: `
      attribute float size;
      attribute vec4 rgba;
      uniform float uScale;
      varying vec4 vC;
      void main() {
        vC = rgba;
        vec4 mv = modelViewMatrix * vec4(position, 1.0);
        gl_PointSize = clamp(size * uScale / -mv.z, 1.0, 72.0);
        gl_Position = projectionMatrix * mv;
      }`,
    fragmentShader: `
      varying vec4 vC;
      void main() {
        float r = length(gl_PointCoord - 0.5);
        if (r > 0.5) discard;
        float a = pow(1.0 - r * 2.0, 1.7);
        float core = smoothstep(0.2, 0.0, r);
        gl_FragColor = vec4(vC.rgb * (a + core * 0.9), vC.a * a);
      }`,
  });
}

function lineMaterial() {
  return new THREE.ShaderMaterial({
    transparent: true,
    depthWrite: false,
    blending: THREE.AdditiveBlending,
    vertexShader: `
      attribute vec4 rgba;
      varying vec4 vC;
      void main() {
        vC = rgba;
        gl_Position = projectionMatrix * modelViewMatrix * vec4(position, 1.0);
      }`,
    fragmentShader: `
      varying vec4 vC;
      void main() { gl_FragColor = vC; }`,
  });
}

/**
 * Buffers for up to maxPoints particles and maxLines lines, added to
 * parent.
 * @param {THREE.Object3D} parent
 * @param {{value: number}} scale
 * @param {number} maxPoints
 * @param {number} maxLines
 */
export function createParticles(parent, scale, maxPoints, maxLines) {
  const pos = new Float32Array(maxPoints * 3);
  const col = new Float32Array(maxPoints * 4);
  const size = new Float32Array(maxPoints);
  const pg = new THREE.BufferGeometry();
  pg.setAttribute('position', new THREE.BufferAttribute(pos, 3).setUsage(THREE.DynamicDrawUsage));
  pg.setAttribute('rgba', new THREE.BufferAttribute(col, 4).setUsage(THREE.DynamicDrawUsage));
  pg.setAttribute('size', new THREE.BufferAttribute(size, 1).setUsage(THREE.DynamicDrawUsage));
  const points = new THREE.Points(pg, pointMaterial(scale));
  points.frustumCulled = false;
  points.renderOrder = 2;

  const lpos = new Float32Array(maxLines * 6);
  const lcol = new Float32Array(maxLines * 8);
  const lg = new THREE.BufferGeometry();
  lg.setAttribute('position', new THREE.BufferAttribute(lpos, 3).setUsage(THREE.DynamicDrawUsage));
  lg.setAttribute('rgba', new THREE.BufferAttribute(lcol, 4).setUsage(THREE.DynamicDrawUsage));
  const lines = new THREE.LineSegments(lg, lineMaterial());
  lines.frustumCulled = false;
  lines.renderOrder = 1;
  parent.add(lines, points);

  let np = 0;
  let nl = 0;

  const api = {
    /** Starts a frame. */
    begin() {
      np = 0;
      nl = 0;
    },

    /** The number of particles so far. */
    get count() {
      return np;
    },

    get lineCount() {
      return nl;
    },

    /**
     * A particle; its index, or -1 when the buffer is full.
     * @param {number} x @param {number} y @param {number} z
     * @param {THREE.Color} c @param {number} a @param {number} s
     */
    point(x, y, z, c, a, s) {
      if (np >= maxPoints) return -1;
      const i = np++;
      pos[i * 3] = x;
      pos[i * 3 + 1] = y;
      pos[i * 3 + 2] = z;
      col[i * 4] = c.r;
      col[i * 4 + 1] = c.g;
      col[i * 4 + 2] = c.b;
      col[i * 4 + 3] = Math.max(0, a);
      size[i] = s;
      return i;
    },

    /** The position of particle i. @param {number} i */
    x: (/** @type {number} */ i) => pos[i * 3],
    y: (/** @type {number} */ i) => pos[i * 3 + 1],
    z: (/** @type {number} */ i) => pos[i * 3 + 2],

    /**
     * A line between particles i and j, in their colours, with alpha a.
     * @param {number} i @param {number} j @param {number} a
     */
    link(i, j, a) {
      if (nl >= maxLines || i < 0 || j < 0 || a <= 0.004) return;
      const k = nl++;
      for (let d = 0; d < 3; d++) {
        lpos[k * 6 + d] = pos[i * 3 + d];
        lpos[k * 6 + 3 + d] = pos[j * 3 + d];
        lcol[k * 8 + d] = col[i * 4 + d];
        lcol[k * 8 + 4 + d] = col[j * 4 + d];
      }
      lcol[k * 8 + 3] = a;
      lcol[k * 8 + 7] = a;
    },

    /**
     * The plexus: links every pair of particles in [from, to) closer
     * than D, fainter the further apart. With groups, only pairs in the
     * same group (index modulo groups) are linked.
     * @param {number} from @param {number} to @param {number} D @param {number} mul
     * @param {number} [groups]
     */
    web(from, to, D, mul, groups = 1) {
      const D2 = D * D;
      for (let i = from; i < to; i++) {
        const xi = pos[i * 3];
        const yi = pos[i * 3 + 1];
        const zi = pos[i * 3 + 2];
        for (let j = i + 1; j < to; j++) {
          if (groups > 1 && (i - from) % groups !== (j - from) % groups) continue;
          const dx = pos[j * 3] - xi;
          const dy = pos[j * 3 + 1] - yi;
          const dz = pos[j * 3 + 2] - zi;
          const d2 = dx * dx + dy * dy + dz * dz;
          if (d2 < D2) api.link(i, j, (1 - Math.sqrt(d2) / D) * Math.min(col[i * 4 + 3], col[j * 4 + 3]) * mul);
        }
      }
    },

    /** Ends a frame: uploads what was drawn. */
    end() {
      pg.setDrawRange(0, np);
      lg.setDrawRange(0, nl * 2);
      for (const k of ['position', 'rgba', 'size']) pg.attributes[k].needsUpdate = true;
      for (const k of ['position', 'rgba']) lg.attributes[k].needsUpdate = true;
    },
  };
  return api;
}

/**
 * A far shell of faint stars, for depth.
 * @param {THREE.Object3D} parent
 * @param {{value: number}} scale
 */
export function starfield(parent, scale) {
  const n = 1800;
  const g = new THREE.BufferGeometry();
  const pos = new Float32Array(n * 3);
  const rgba = new Float32Array(n * 4);
  const size = new Float32Array(n);
  let s = 11;
  const rnd = () => (s = (s * 16807) % 2147483647) / 2147483647;
  const tint = new THREE.Color('#9fc3ff');
  const white = new THREE.Color('#dfe8ff');
  for (let i = 0; i < n; i++) {
    const u = rnd() * 2 - 1;
    const a = rnd() * Math.PI * 2;
    const r = 700 + rnd() * 500;
    const q = Math.sqrt(1 - u * u);
    pos.set([Math.cos(a) * q * r, u * r, Math.sin(a) * q * r], i * 3);
    const c = rnd() < 0.2 ? tint : white;
    rgba.set([c.r, c.g, c.b, 0.55 * (0.25 + 0.75 * rnd() ** 2)], i * 4);
    size[i] = 5 + rnd() * 9;
  }
  g.setAttribute('position', new THREE.BufferAttribute(pos, 3));
  g.setAttribute('rgba', new THREE.BufferAttribute(rgba, 4));
  g.setAttribute('size', new THREE.BufferAttribute(size, 1));
  const p = new THREE.Points(g, pointMaterial(scale));
  p.frustumCulled = false;
  parent.add(p);
  return p;
}
