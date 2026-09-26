# Which 2.5D rendering approach suits Argus in a browser and on a wall TV?

Research for [#99](https://github.com/Itema-as/iidp/issues/99), part of the Argus map [#97](https://github.com/Itema-as/iidp/issues/97). Researched 2026-09-26.

**The scene.** An island (the Platform) with tens of districts (Applications). Each district has buildings (Environments) with attached structures (Capabilities), plus island infrastructure (Platform components). That is fewer than about 200 objects. The scene needs pan/zoom, hover, click-to-select, a camera that flies to an object, state animations (pulse, scaffolding, smoke) and Helios-style beads along Deploy/Promote paths. Desktop browsers come first. A wall TV comes second. The frontend is static files embedded in a Go binary, and nothing runs Node in production.

**Scale matters.** At fewer than 200 objects, every option below runs well within its limits on a desktop. The choice depends on four other things:

1. Whether the wall device can run the renderer at all.
2. How much code hover, labels, the camera and accessibility cost.
3. Bundle size.
4. Whether a JS toolchain has to enter this Go repository.

## Summary table

| | Helios-style (canvas + SVG, no WebGL) | Plain SVG isometric (recommended) | PixiJS v8 (2D WebGL) | three.js + orthographic camera |
|---|---|---|---|---|
| Needs WebGL | No (it only probes WebGL to detect the GPU) | No | No. Uses WebGL2, then WebGL1, then an experimental Canvas renderer | **Yes, WebGL2 only** since r163 |
| Library size, min / gzip (measured) | Helios's own bundle is 978 / 291 KiB, but it isn't a library | 0, or about 50 / 17 KiB with d3-zoom | 810 / 228 KiB as a vendored file. 559 / 163 KiB tree-shaken. +pixi-viewport gives 609 / 173 KiB | 725 / 184 KiB minified. 556 / 140 KiB tree-shaken for a minimal orthographic scene. The npm files are unminified (407 KiB gzip) |
| Hover and picking | DOM events on SVG and HTML | **Native**: DOM events per element | Built in: `eventMode`, `hitArea` | `Raycaster` on each pointer move (a small amount of code) |
| Fly-to camera | Not in Helios (orbit around a fixed home) | Tween one `transform`. d3-zoom does van Wijk smooth zoom | Tween the container, or `pixi-viewport` `animate()` | `camera-controls` `setLookAt(…, true)` / `fitToBox` |
| Beads on paths | SMIL `<animateMotion>` | SMIL or `getPointAtLength` plus rAF | Per-tick sprite position | `Curve.getPointAt` plus a mesh |
| Text labels | HTML chips placed at projected points | Native `<text>` or HTML, sharp at any zoom | `Text` / `BitmapText` textures, re-rasterised on zoom, or a DOM overlay | `CSS2DRenderer` (DOM) or a text add-on |
| Accessibility | HTML chips with `role="button"` | Real elements: `role`, `aria-label`, `tabindex`, `<title>` | Opt-in `AccessibilitySystem` DOM overlay | Nothing built in. You make CSS2D labels into buttons yourself |
| No build step | n/a (Helios uses Vite) | **Yes**. Plain ES modules | Yes for `pixi.min.mjs` (no imports inside). pixi-viewport needs an import map | Only with an import map (Chrome 89+). Addons import the bare name `three` |
| Wall TV risk | GPU compositing bugs on entry GPUs. Helios keeps three render paths | Lowest. CPU cost grows with how many things animate (unconfirmed per device) | Low to medium. It has fallbacks, but the Canvas one is experimental | **Highest**. No WebGL on Cast receivers. No software fallback in desktop Chrome 139+ |

## 1. Helios: tilted canvas plus SVG overlays, no WebGL

Source read: [ReikanYsora/Helios](https://github.com/ReikanYsora/Helios) at commit `37d6ce3`, v2026.9.6. The main files are `ARCHITECTURE.md`, `src/scene/renderer.ts`, `src/scene/projection.ts`, `src/scene/helios-engine.ts` and `src/hud/scene-hud-controller.ts`.

**What it does.**

- **Basemap.** Helios decodes OpenFreeMap vector tiles and paints them onto an HTML `<canvas>` once. After that it tilts the canvas with a single CSS `rotateX(pitch) rotateZ(bearing)` transform and never redraws it per frame ([ARCHITECTURE.md §2 "Ground plane"](https://github.com/ReikanYsora/Helios/blob/37d6ce316a1b1edaa7e37988788b66a5295d008a/ARCHITECTURE.md)). There are three canvases at different levels of detail.
- **Buildings and shadows.** These are not in the canvas. A screen-space `<svg>` is repainted every frame. `SceneCamera.project(east, north, up)` maps local metres to pixels through bearing, pitch and perspective, as the exact inverse of the canvas's CSS transform. That is what keeps the SVG "welded" to the ground. Faces are depth-sorted with the painter's algorithm. The `<svg>` keeps one node per shape between frames and rewrites only the attributes that changed ([`projection.ts`](https://github.com/ReikanYsora/Helios/blob/37d6ce316a1b1edaa7e37988788b66a5295d008a/src/scene/projection.ts), ARCHITECTURE.md "Scene SVG").
- **Scale.** Up to 100 buildings plus their cast shadows, set by `building-count` 10 to 100 ([docs/CONFIGURATION.md](https://github.com/ReikanYsora/Helios/blob/37d6ce316a1b1edaa7e37988788b66a5295d008a/docs/CONFIGURATION.md)). That is the same order of magnitude as Argus.
- **Beads.** Each is a `<circle>` with a native SVG SMIL `<animateMotion>` along the leader path, "not a JS/CSS animation". The attributes are guarded so that re-renders don't restart the SMIL clock ([`scene-hud-controller.ts` ~L840–970](https://github.com/ReikanYsora/Helios/blob/37d6ce316a1b1edaa7e37988788b66a5295d008a/src/hud/scene-hud-controller.ts)). Animations pause off-screen through `svg.pauseAnimations()`, and `prefers-reduced-motion` turns them off.
- **Interaction.** Dragging orbits the camera (bearing and pitch). There is an auto-rotate loop. Zoom is a fixed `scene-zoom` of 1, 1.5 or 2. The camera always aims at the home origin: there is **no pan and no fly-to**. The clickable things are absolutely positioned **HTML chips** (`role="button"`, `tabindex="0"`), placed each frame at projected coordinates with `transform: translate()`. The buildings themselves are not interactive.

**"No WebGL" did not mean "no GPU trouble".** Helios's `renderer.ts` still creates a throwaway WebGL context to read the GPU name and `MAX_TEXTURE_SIZE`. It then chooses between three ground modes:

- `normal`: a GPU canvas under the CSS 3D transform.
- `transform`: a CPU-rasterised canvas under the transform.
- `projected`: a per-frame CPU reprojection with no 3D layer.

The reason is that entry-level Mali, Adreno 2xx–6xx, PowerVR and VideoCore/V3D (Raspberry Pi) GPUs "corrupt a GPU-rasterized canvas into colored noise" or mis-composite the tilted layer. Kiosk WebViews that hide the GPU name are treated as suspect, and so is old iOS ([`renderer.ts` L37–190](https://github.com/ReikanYsora/Helios/blob/37d6ce316a1b1edaa7e37988788b66a5295d008a/src/scene/renderer.ts)). The CHANGELOG has several entries about flicker, black kiosk screenshots and drops to a few frames per second on "low-end wall tablets", each fixed by another fallback. Helios had to engineer this because the **CSS 3D tilt** of a large canvas is the fragile part. A fixed-angle isometric view doesn't need that tilt at all.

**Reusable, or only inspiration?** Only inspiration.

- **Licence.** The licence is **GPL-3.0-or-later** (`LICENSE` and `package.json`). Copying its code into Argus would make the Argus frontend a GPL-3.0 derivative. Reading it and re-implementing the ideas (project to screen, SVG prisms sorted back to front, SMIL beads) is fine. This is not legal advice.
- **Not a library.** `package.json` is `"private": true` and it isn't on npm. It is a Lit web component tied to Home Assistant's `hass` object, OpenFreeMap tiles and solar maths. Most of the 37.6k lines of TypeScript solve problems Argus doesn't have (map tiles, levels of detail, sun shadows, weather).
- **Toolchain.** It is built with Vite into a single 978 KiB file (291 KiB gzip, measured), so it isn't a no-build-step example either.

## 2. Plain SVG isometric (Helios's overlay idea without the tilted canvas)

This is what remains of Helios once you drop the map. It fits Argus better than Helios does, because Argus's world is invented. A **fixed isometric angle** (no free rotation) makes the whole scene a 2D drawing:

- Every object is projected once, when the layout changes.
- The depth order is static.
- Pan and zoom are one affine `transform` on a root `<g>`, or the `viewBox`.

With no CSS 3D layer, the GPU compositing class that Helios fights never comes up. If limited rotation is wanted later, Helios shows that re-projecting and re-sorting about 100 prisms per frame in SVG works on phones and wall tablets. Treat that as the author's claim, supported by the code: the README says "Light enough to stay fluid on a phone, a tablet or an old wall panel".

- **Hover and click.** Native `pointerenter` and `click` on each building's `<g>`. There is nothing to write for hit-testing, and the hit area follows the real shape.
- **Camera.** Interpolate `(cx, cy, scale)` and write one transform per frame. d3-zoom handles wheel, drag and pinch, including browser quirks. Its transitions use van Wijk and Nuij's "Smooth and efficient zooming and panning" (`interpolateZoom`), which is the "fly to" feel. It is "agnostic about the DOM, so you can use it with SVG, HTML or Canvas" ([d3-zoom README](https://github.com/d3/d3-zoom), [d3-interpolate `interpolateZoom`](https://github.com/d3/d3-interpolate#interpolateZoom)). Size: 49.9 / 16.9 KiB min/gzip for zoom, selection and transition (measured). A hand-written version is roughly 100 lines.
- **Beads.** SMIL `<animateMotion>` along the path, exactly as Helios does, or `path.getPointAtLength()` in a `requestAnimationFrame` loop.
- **State animations.** CSS keyframes and SMIL on classes (`.is-deploying`, `.is-degraded`): pulse, blinking scaffolding, smoke puffs. `prefers-reduced-motion` and a wall-mode "calm" switch can turn them off. Helios does the same.
- **Labels.** `<text>`, or HTML placed over projected anchor points. Either stays sharp at any zoom.
- **Accessibility.** The best of the four. Every building can be a focusable element with `role`, `aria-label` and a state description, and the SVG can sit next to a plain list or table view. The other options have to simulate this.
- **No build step.** Plain `<script type="module">` files embedded with `go:embed`. You can vendor d3 as one UMD file (`d3.min.js`, 273 / 90 KiB) or write the camera yourself.
- **Performance risk.** SVG is rasterised on the CPU in the paint path. Many animations running at once, with drop shadows or filters, at 1080p on a weak TV CPU could drop frames. **Unconfirmed**: I found no primary benchmark of SVG on smart-TV browsers at this scale, and I couldn't test on a device. Mitigations: avoid SVG filters, animate `transform`/`opacity`, cap the number of concurrent effects in wall mode, and pause animations off-screen.

## 3. PixiJS v8: 2D WebGL used isometrically

PixiJS 8.21.0 (2026-09-17), MIT licence.

- **Renderer fallbacks.** `autoDetectRenderer` picks WebGL first. The WebGL renderer prefers WebGL2 but accepts `preferWebGLVersion: 1`. Since **v8.16.0 (2026-02-03)** there is a Canvas 2D renderer again, labelled "experimental, please let us know if you run into any issues" in the [v8.16.0 release notes](https://github.com/pixijs/pixijs/releases/tag/v8.16.0). You choose it with `preference: ['webgl', 'canvas']`, and later releases keep fixing Canvas-renderer bugs. Verified in the package's `autoDetectRenderer.d.ts` and `GlContextSystem.d.ts`. So PixiJS can survive a device with no WebGL, but only on a young code path.
- **Size (measured).**
  - `dist/pixi.min.mjs` is 809.5 KiB minified, 227.8 KiB gzip. It has **no import statements and no dynamic `import()`**, so you can vendor it as one file and import it by relative path with no bundler.
  - A tree-shaken esbuild bundle of `Application` + `Graphics` + `Text` + `BitmapText` + events is 558.6 / 162.8 KiB.
  - Adding `pixi-viewport` 6.0.3 gives 608.6 / 173.3 KiB.
- **Hover and picking.** Built in. `eventMode = 'static'`, `hitArea` for precise shapes (an isometric diamond footprint as a `Polygon`), `cursor`, and `pointerover`/`pointertap` ([PixiJS events guide](https://pixijs.com/8.x/guides/components/events)).
- **Camera.** `pixi-viewport` provides drag, pinch, wheel, deceleration and `animate({ position, scale, time, ease })`, `snap()`, `snapZoom()` and `follow()` (checked in its `.d.ts`). It targets `pixi.js >=8`, but the repository was last pushed 2025-02-03 and the last release was 2024-11-27, so treat it as a maintenance risk. It also imports the bare name `pixi.js`, so it needs an import map (Chrome 89+) or a bundler. Tweening a root `Container` yourself is simple.
- **Beads and animations.** Move sprites along precomputed polylines in the app ticker. It is cheap, and GPU batching handles thousands of particles, far more than Argus needs.
- **Labels.** `Text` rasterises to a texture and blurs when zoomed unless re-rendered at a higher resolution. `BitmapText` or `HTMLText` are the alternatives. The simplest option is a DOM label overlay, as Helios does.
- **Accessibility.** An opt-in `AccessibilitySystem` (`import 'pixi.js/accessibility'`) lays focusable DOM elements over accessible objects, with `accessibleTitle`, `accessibleHint` and `tabIndex` ([PixiJS accessibility guide](https://pixijs.com/8.x/guides/components/accessibility)). It works, but it is a simulation of what SVG gives natively.
- **What it buys Argus.** Headroom for thousands of sprites, particles and filters, and GPU compositing of a full-screen scene. At fewer than 200 objects, Argus doesn't need that headroom.
- **Alternative.** Konva 10.7.0 (MIT, 188 / 56 KiB, Canvas 2D only) gives Pixi-like shape events on Canvas 2D through a hidden "hit graph" canvas, with no WebGL dependency ([Konva performance docs](https://konvajs.org/docs/performance/All_Performance_Tips.html)). It is worth knowing about if SVG turns out too slow on a TV CPU and WebGL is not available there.

## 4. three.js with an orthographic camera

three.js 0.186.1 (r186), MIT licence.

- **WebGL2 only.** `WebGLRenderer` throws `'THREE.WebGLRenderer: WebGL 1 is not supported since r163.'` (`src/renderers/WebGLRenderer.js`, L61 and L102). It has no Canvas 2D fallback. This is the decisive fact for a TV.
- **Size (measured).**
  - The npm package ships **no minified build** in r186. `build/three.module.js` imports `./three.core.js`, and together they are 2.0 MiB raw / 407.5 KiB gzip unminified.
  - Minified with esbuild: 724.9 / 184.2 KiB.
  - A tree-shaken minimal scene (orthographic camera, Lambert and Basic materials, lights, `Raycaster`, `CatmullRomCurve3`, `CSS2DRenderer`, `MapControls`) is still 556.0 / 140.0 KiB, because `WebGLRenderer` pulls in its whole shader library.
- **No build step.** The official manual's "Option 2: Import from a CDN" uses an **import map** for `three` and `three/addons/` ([three.js installation manual](https://github.com/mrdoob/three.js/blob/dev/manual/pages/installation.html)). The addons (`MapControls`, `CSS2DRenderer`) `import … from 'three'` by bare name, so vendoring them needs an import map (Chrome 89+) or a bundler. The manual calls npm plus a build tool "the recommended approach for most users".
- **Hover and picking.** `Raycaster.setFromCamera` + `intersectObjects` on pointer move. That is a few lines and fast at 200 objects. You add the hover-state and cursor logic yourself.
- **Camera.** [`camera-controls`](https://github.com/yomotsu/camera-controls) 3.1.2 (MIT, active: pushed 2026-09-09, 61 / 12 KiB min/gzip) gives `setLookAt(…, enableTransition)`, `moveTo`, `zoomTo` and `fitToBox(object, true)`. That is fly-to in one call. Built-in `MapControls` does pan and zoom but doesn't tween.
- **Beads.** `CatmullRomCurve3.getPointAt(t)` driving small meshes, or `InstancedMesh`.
- **Labels.** `CSS2DRenderer` places DOM elements at 3D positions. Per the docs it "only supports 100% browser and display zoom" ([CSS2DRenderer docs](https://threejs.org/docs/pages/CSS2DRenderer.html)). SDF text in WebGL needs another library.
- **Accessibility.** Nothing built in. Labels can be DOM buttons, and you build a parallel list yourself.
- **What it buys Argus.** Real 3D models with lighting and shadows, and free rotation "for free". It also brings a modelling or asset pipeline (glTF) that the other options don't need.
- **Declarative wrappers.** `@react-three/fiber` 9.8.1 (peer: `react`, `react-dom`), Threlte 8.6.1 (peer: `svelte`) and TresJS 5.9.0 (peer: `vue`) all need a JSX, Svelte or Vue compiler, which means a bundler and Node at build time, plus a UI framework. A-Frame 1.8.0 is declarative HTML with no build step, but it is VR-oriented and carries all of three.js. None of them improves the TV picture.

## 5. The wall TV: what the platforms say

- **Chromecast / Google Cast Web Receiver.** "The Web Receiver is a Chrome browser optimized for video playback. As such, WebGL and Chrome Native Client (NaCL) are not currently supported". Also: "The Cast device is a low-power device with memory, CPU and GPU limitations, so the Web Receiver application should be as lightweight as possible" ([Cast UX guidelines](https://developers.google.com/cast/docs/ux_guidelines), updated 2024-10-24). So **three.js cannot run as a Cast receiver at all**, and PixiJS would have to use its experimental Canvas path there. Tab-casting from a desktop Chrome renders on the sender, so this limit doesn't apply to it.
- **Google TV / Android TV sticks.** These run a browser app or kiosk WebView on entry Mali or PowerVR GPUs. That is the GPU family Helios identified as corrupting GPU canvases and mis-compositing 3D layers (above). **Unconfirmed:** which GPU a given stick has, and whether a browser is installed by default. I didn't find a primary Google spec page listing the GPU.
- **Samsung Tizen TVs.**
  - The built-in web engine is Chromium M69 (2020), M76 (2021), M85 (2022), M94 (2023), M108 (2024), M120 (2025) and M130 (2026).
  - Samsung lists "WebGL API (Canvas 3D)" as supported for every year, without saying WebGL 1 or 2 ([Samsung web engine specifications](https://developer.samsung.com/smarttv/develop/specifications/web-engine-specifications.html)).
  - Import maps shipped in Chrome 89, so Tizen TVs before 2023 lack them. ES modules (Chrome 61) are missing on 2018 models (M56).
- **LG webOS TVs.**
  - The engine is Chromium 68 (2020), 79 (2021), 87 (2022), 94 (2023), 108 (2024), 120 (2025) and 132 (2026) ([webOS web engine](https://webostv.developer.lge.com/develop/specifications/web-api-and-web-engine)).
  - The page says nothing about WebGL. **Unconfirmed:** WebGL2 availability and GPU limits on webOS.
- **A cheap mini PC running desktop Chrome.**
  - Desktop Chrome removed the automatic SwiftShader software fallback for WebGL. Per [chromestatus](https://chromestatus.com/feature/5166674414927872) it is deprecated with a desktop milestone of 139: "WebGL context creation will fail instead of falling back to SwiftShader … Chromium and other browsers do not guarantee WebGL availability. Please test and handle WebGL context creation failure and fall back to other web APIs such as Canvas2D".
  - [web3dsurvey](https://web3dsurvey.com/) reports WebGL2 on about 97% of devices overall but only **86–87% on Linux** (no date given). A Linux kiosk box with a blocklisted GPU gets a blank WebGL scene.
  - An x86 mini PC with an Intel iGPU is the most predictable wall device of all the options. **Unconfirmed:** no specific model was tested.
- **Resolution.** On a 4K TV, every approach should cap the pixel ratio at 1 in wall mode (`resolution` in PixiJS, `setPixelRatio` in three.js, and CSS pixels are already capped for SVG). **Unconfirmed:** how much this matters per device.

**In short:** only the options that don't depend on WebGL (SVG, Canvas 2D, and PixiJS on its Canvas path) run on every wall device class named in #97. Even without WebGL, keep away from large CSS 3D-transformed layers on entry GPUs, which was Helios's lesson.

## 6. Cost of a JS toolchain in this Go repository

Today this repo builds with `go` alone. `ci.yaml` runs gofmt, vet and test, and `.goreleaser.yaml` builds `cmd/*` with `CGO_ENABLED=0` and a Deploy gate image from its own Dockerfile.

`go:embed` needs the embedded files to exist at compile time, including for `go vet ./...` and `go test ./...`. A generated bundle therefore has to be committed, or produced before every Go step: in CI, in GoReleaser's `before.hooks`, in the component's Dockerfile and on developers' machines. The options, cheapest first:

1. **No build.** Hand-written ES modules, plus any vendored library as a committed file (`pixi.min.mjs` is self-contained). Nothing changes in CI or GoReleaser. You lose TypeScript, unless you use JSDoc types checked by a separate optional step.
2. **esbuild through its Go API.** esbuild is written in Go and is a Go module (`github.com/evanw/esbuild` v0.28.2, the same version measured here; [esbuild getting started](https://esbuild.github.io/getting-started/) shows `import "github.com/evanw/esbuild/pkg/api"`). A `go generate` or `go run` step can strip TypeScript and bundle with no Node. The catch is that `go generate` isn't run by `go build`, so the output must still be committed or run in CI. Libraries would be vendored files rather than npm packages, and the Go API doesn't support esbuild's JS plugins.
3. **npm + Vite/esbuild.** This adds `setup-node`, a lockfile, a `node_modules` cache, dependency-update bots and a `before.hooks` step. It is the only route if a declarative wrapper (React Three Fiber, Threlte, TresJS) is wanted.

## Recommendation

**Build Argus as a fixed-angle isometric SVG scene in plain ES modules, embedded with `go:embed`, with no WebGL and no Node toolchain.**

- **Layout.** Project the island's tile coordinates to 2D once per layout change and draw each building and Capability as a small group of SVG polygons sorted back to front (Helios's prism idea, re-implemented, not copied, because of the GPL).
- **Camera.** Pan, zoom and fly-to are one tweened transform using van Wijk-style interpolation: vendored d3-zoom (about 17 KiB gzip) or about 100 lines of our own.
- **Animations.** Beads are SMIL `<animateMotion>` or `getPointAtLength`, exactly as Helios does them. State animations are CSS and SMIL classes, with `prefers-reduced-motion` and a calm wall mode.
- **Hover, click and accessibility.** Native DOM events. Label objects with real `aria-label`/`role`/`tabindex`, and provide a list view next to the scene.

Why:

- It is the only option that runs on every wall device class without a fallback path. Cast receivers have no WebGL, and desktop Chrome 139+ has no software WebGL.
- It needs no JS toolchain in CI or GoReleaser.
- It is the smallest (0 to 17 KiB of library).
- It makes hover, labels and accessibility free rather than simulated.
- At fewer than 200 objects, the headroom WebGL offers isn't needed.

**Keep a way out.** Separate the scene model (projected shapes with depth and state) from the painter, so that a Canvas 2D or PixiJS painter (vendored `pixi.min.mjs`, `preference: ['webgl', 'canvas']`) can replace the SVG one. Do that only if the #102 prototype, measured on the actual wall device, drops frames.

**Don't choose three.js** unless free rotation or lit 3D models become a hard requirement. It is WebGL2-only with no fallback, the largest bundle (140 KiB gzip at minimum), and needs an import map or bundler, an asset pipeline, and hand-built labels and accessibility.

**For the wall itself**, prefer a small x86 mini PC running desktop Chrome in kiosk mode over a Chromecast receiver or a TV's built-in browser. Check the frame rate there as part of the prototype.

## Unconfirmed, and where I looked

- **Frame rates on real smart-TV, Google TV or mini-PC browsers**, for any of the four approaches. No device was available. I found no primary benchmark of SVG, Pixi or three.js on TV browsers at this scale. Looked at: vendor TV docs (Samsung, LG, Google Cast), library docs, and Helios's CHANGELOG, which describes wall tablets, not TVs.
- **WebGL 1 vs 2 and GPU limits on Tizen and webOS TVs.** Samsung lists "WebGL API" without a version. LG's web engine page doesn't mention WebGL.
- **Which GPU the Chromecast with Google TV and the Google TV Streamer use, and whether they ship a browser.** No primary Google spec found.
- **Helios's claim that it stays fluid "on an old wall panel".** This is the author's statement. The code supports it (many fallbacks, SVG node reuse), but I didn't measure it.
- **Whether SVG child animations are GPU-composited in Chromium.** I assumed they are CPU-painted, without a primary source.
- **pixi-viewport's future maintenance.** Inferred from the repository's last-push date (2025-02-03) only.

## How the sizes were measured

The npm tarballs were fetched from registry.npmjs.org: three 0.186.1, pixi.js 8.21.0, pixi-viewport 6.0.3, camera-controls 3.1.2, konva 10.7.0, d3 7.9.0 and d3-zoom 3. Bundles were built with esbuild 0.28.2 (`--bundle --minify --format=esm`). Gzip sizes use Python `gzip.compress(level=9)`. The entry files imported only what each row names. Helios's size is its committed `dist/helios.js`.

## Sources

- Helios source, commit `37d6ce3`: [ARCHITECTURE.md](https://github.com/ReikanYsora/Helios/blob/37d6ce316a1b1edaa7e37988788b66a5295d008a/ARCHITECTURE.md), [src/scene/renderer.ts](https://github.com/ReikanYsora/Helios/blob/37d6ce316a1b1edaa7e37988788b66a5295d008a/src/scene/renderer.ts), [src/scene/projection.ts](https://github.com/ReikanYsora/Helios/blob/37d6ce316a1b1edaa7e37988788b66a5295d008a/src/scene/projection.ts), [src/hud/scene-hud-controller.ts](https://github.com/ReikanYsora/Helios/blob/37d6ce316a1b1edaa7e37988788b66a5295d008a/src/hud/scene-hud-controller.ts), [CHANGELOG.md](https://github.com/ReikanYsora/Helios/blob/37d6ce316a1b1edaa7e37988788b66a5295d008a/CHANGELOG.md), [LICENSE (GPL-3.0)](https://github.com/ReikanYsora/Helios/blob/37d6ce316a1b1edaa7e37988788b66a5295d008a/LICENSE)
- Google Cast, [UX guidelines](https://developers.google.com/cast/docs/ux_guidelines): no WebGL on Web Receivers
- Chrome Platform Status, [Remove SwiftShader fallback](https://chromestatus.com/feature/5166674414927872)
- [Samsung Tizen TV web engine specifications](https://developer.samsung.com/smarttv/develop/specifications/web-engine-specifications.html); [LG webOS TV web engine](https://webostv.developer.lge.com/develop/specifications/web-api-and-web-engine)
- [web3dsurvey.com](https://web3dsurvey.com/): WebGL/WebGL2 support by platform
- three.js: `src/renderers/WebGLRenderer.js` in the 0.186.1 package (WebGL1 dropped in r163); [installation manual](https://github.com/mrdoob/three.js/blob/dev/manual/pages/installation.html); [CSS2DRenderer docs](https://threejs.org/docs/pages/CSS2DRenderer.html); [camera-controls](https://github.com/yomotsu/camera-controls)
- PixiJS: [v8.16.0 release notes (Canvas renderer)](https://github.com/pixijs/pixijs/releases/tag/v8.16.0); `autoDetectRenderer.d.ts` and `GlContextSystem.d.ts` in the 8.21.0 package; [events guide](https://pixijs.com/8.x/guides/components/events); [accessibility guide](https://pixijs.com/8.x/guides/components/accessibility); [pixi-viewport](https://github.com/pixijs-userland/pixi-viewport)
- [d3-zoom README](https://github.com/d3/d3-zoom); [d3-interpolate `interpolateZoom`](https://github.com/d3/d3-interpolate#interpolateZoom)
- [Konva performance tips](https://konvajs.org/docs/performance/All_Performance_Tips.html)
- [esbuild getting started (Go API)](https://esbuild.github.io/getting-started/)
