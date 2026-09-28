# three.js, vendored

Argus draws with [three.js](https://threejs.org) **r170**, the npm package `three@0.170.0`, vendored here so the page loads nothing from outside the Platform (#115, #119). There is no build step: `index.html`'s import map points `three` at `three.module.min.js` and `three/addons/` at `addons/`, and the binary embeds this directory with `go:embed`.

- Source: `https://registry.npmjs.org/three/-/three-0.170.0.tgz`, whose SHA-512 matches the registry's `dist.integrity`, `sha512-FQK+LEpYc0fBD+J8g6oSEyyNzjp+Q7Ks1C568WWaoMRLW+TkNNWmenWeGgJjV105Gd+p/2ql1ZcjYvNiPZBhuQ==`. The tarball's own SHA-256 is `4a608a355dcaba72e0e5383cdc814303f5b6060b43c238cdf6932dceb699238d`.
- Licence: MIT, in `LICENSE`, copied from the package unchanged.
- Nothing here is edited. Only the files Argus imports are kept: the core, and the six addons (`OrbitControls`, `EffectComposer`, `RenderPass`, `UnrealBloomPass`, `OutputPass`, `CSS2DRenderer`) with the passes and shaders they import themselves.

| File | From the package | SHA-256 |
|---|---|---|
| `three.module.min.js` | `build/three.module.min.js` | `08fd7545d13d2c7fb65ab691530a802dafefd638596501854f267d0fb13c39e7` |
| `LICENSE` | `LICENSE` | `4c40a1ef62450b857c3b2aaf294936304cd552d965fbcd9d32d4c5bcf4ba4454` |
| `addons/controls/OrbitControls.js` | `examples/jsm/controls/OrbitControls.js` | `80efaadea4f8a636a65fb0bd08bfef62f3d93a0bb94e2e7500f23176c5c07f4e` |
| `addons/postprocessing/EffectComposer.js` | `examples/jsm/postprocessing/EffectComposer.js` | `d234e578618fa816955ebdc059c049c577e203e650e33cf22bde3f232c29e669` |
| `addons/postprocessing/MaskPass.js` | `examples/jsm/postprocessing/MaskPass.js` | `328cf7db0da5d9be83ffe39d54b01d5ac1fddf108cc98182ddbb056f5c8b537f` |
| `addons/postprocessing/OutputPass.js` | `examples/jsm/postprocessing/OutputPass.js` | `32f879d2179087676631c799857a885586b3cdd13b9731bd3b13f06428bd58b7` |
| `addons/postprocessing/Pass.js` | `examples/jsm/postprocessing/Pass.js` | `b3c6128340eaa37e40a6a2f1b738e894c855239417d50959759b34a2b5e89f92` |
| `addons/postprocessing/RenderPass.js` | `examples/jsm/postprocessing/RenderPass.js` | `6c9b8a539ea16e898f65e4760f14937ef9ea94043bd9842c141e0301f41903e8` |
| `addons/postprocessing/ShaderPass.js` | `examples/jsm/postprocessing/ShaderPass.js` | `3b28a1ee27e0eb96c0eab137a1f442ccf127a926904eced2d51e125ec44af781` |
| `addons/postprocessing/UnrealBloomPass.js` | `examples/jsm/postprocessing/UnrealBloomPass.js` | `3bd23a1097af75c7002d0ffc21a6c14f45c4dd701dbaf737030dfc61fb7c64d9` |
| `addons/renderers/CSS2DRenderer.js` | `examples/jsm/renderers/CSS2DRenderer.js` | `7de0bb70e3c1d6da58416353ed7140a7a7743ece99d73b56eb62bc2dd79bfed5` |
| `addons/shaders/CopyShader.js` | `examples/jsm/shaders/CopyShader.js` | `4e3346db194db56a596cd074e9bdb39fb5eb52040c333e0d29dc4eb1324d3b1d` |
| `addons/shaders/LuminosityHighPassShader.js` | `examples/jsm/shaders/LuminosityHighPassShader.js` | `9f4866f9abb2d96fd83eec46ba4bf2165b22155a7a37ff425c0f60eba18007cb` |
| `addons/shaders/OutputShader.js` | `examples/jsm/shaders/OutputShader.js` | `4944cecd49c0d4d1520a4d927bde8a590fd43f041ee913252b9451855a01d0f0` |

To check: `shasum -a 256 -c` against the table, or download the tarball and compare each file with `cmp`.

To upgrade: download the new tarball, check it against the registry's `dist.integrity`, copy the same files (and any new file the addons import, found with `grep -n "from '" addons/**/*.js`), update this table and the version in `index.html`'s comment, and check the page in a browser.
