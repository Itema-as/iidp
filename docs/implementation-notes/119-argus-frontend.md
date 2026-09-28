# #119 Argus's frontend: the particle network, the camera and the detail card

This note records the decisions taken while building Argus's page, `cmd/iidp-argus/web`, on the stream of [#118](118-argus-backend.md). The design is the spec's ([#115](https://github.com/Itema-as/iidp/issues/115): "The drawing", "The camera", "The detail card"), from the particle-network decision on the map ([#97](https://github.com/Itema-as/iidp/issues/97)), the camera decision ([#103](https://github.com/Itema-as/iidp/issues/103)) with the Deploy-tracing decision's loudness and beads ([#106](https://github.com/Itema-as/iidp/issues/106)), the states ([#100](https://github.com/Itema-as/iidp/issues/100)) and the detail card, panel B ([#113](https://github.com/Itema-as/iidp/issues/113)). The look and behaviour are the prototype's, `prototype-network-three.html?panel=B` on `adriansberg/prototype-argus-island`, rewritten rather than copied. It also records the small backend additions the card needed, and how all of it was checked.

Sources, checked on 2026-09-28:

- three.js **r170**, the npm package `three@0.170.0`, read in the tarball: `build/three.module.js` imports nothing, so one file is the whole core; each addon's own imports (the list in `vendor/three/README.md`); and `examples/jsm/renderers/CSS2DRenderer.js`, which hides a label with `display: none` only for `visible === false` or outside the view, so hiding a crowded label is left to the page (below).
- The behaviour of `EventSource` and `fetch` after the Itema login session expires, checked in Chrome 153 against the demo (below), which answers as oauth2-proxy does: the stream's reconnect gets a 302 to the sign-in page, the cross-origin redirect fails the `EventSource` for good (`readyState` `CLOSED`), and `fetch(..., {redirect: 'manual'})` of the same page reports an `opaqueredirect`.

## The page

The page is plain ES modules with no build step: `index.html` has an import map, `js/main.js` is its module, and `go:embed` puts the whole `web` directory in the binary, as #118 set up. Every hand-written module starts with `// @ts-check` and types itself with JSDoc; `js/types.js` holds the stream's shapes from `internal/argus/doc.go`.

| Module | What it does | three.js? |
|---|---|---|
| `js/model.js` | The model: the domain objects from the stream, replaced whole as they come, plus each Application's orbit slot and the times the page saw things happen | no |
| `js/stream.js` | The `EventSource`, and what to do when it stops (below) | no |
| `js/attention.js` | The automatic camera's rules: the queue, loudness, lingering, bursts, holding still, and the chip's words | no |
| `js/feed.js` | The feed as the server keeps it, and the map's six | no |
| `js/deploys.js` | The hop strip, and where each Deploy's bead is on its route | no |
| `js/look.js` | Which look and tag each state takes | no |
| `js/card.js` | What the card shows for an Environment, a component and the core | no |
| `js/layout.js` | Where things sit: orbit slots, clouds, the component ring | no |
| `js/labels.js` | Which labels show where they would pile up | no |
| `js/draw/drawing.js` | The seam (below) | yes |
| `js/draw/stage.js` | The renderer and its glow, the labels' renderer, the camera, OrbitControls and the camera rig | yes |
| `js/draw/network.js` | The particle network | yes |
| `js/draw/particles.js` | The particle and line buffers and their shaders | yes |
| `js/ui/*.js` | The card, the chip, the feed, the edge markers, the banner and the frame-rate meter, in the DOM | no |

**The seam.** `createDrawing(container, {reduced, onInput})` is everything the rest of the page asks of the drawing: `frame(t, view)` draws a frame from the model; `where(key)` says where an Application (or the Platform) is; `placeOf(target)` and `screenRadius(target)` where a thing is; `pick(x, y)` what is under the pointer; `project(p)` where a point is on the screen; and `camera.home()`, `visit(key)`, `wide(keys)`, `hold()`, `go(key)` and `show(target)` fly the camera. A second drawing, such as the islets, would implement the same and nothing else would change.

**Animations run from the model's times** (#115): the model records when the page saw an Environment arrive, start leaving and go, an Application go, and a Deploy reach each hop, be superseded or be refused. The drawing computes every frame from those and the clock, so a snapshot or a redraw never restarts one. Something the page found in a snapshot, rather than saw happen, is drawn as already there: a Deploy found at Applying holds its bead at the hub rather than travelling, and a refusal or a Serving flash from before the page opened is not played.

**Slots are kept for life** (#102): the first snapshot gives slots in name order; a new Application takes the lowest free slot; a gone one sinks and frees its slot for the next. A reconnect's fresh snapshot keeps the slots of the Applications still there, so nothing already drawn moves.

## Vendored three.js

`web/vendor/three` holds `three.module.min.js`, the six addons the page uses (OrbitControls, EffectComposer, RenderPass, UnrealBloomPass, OutputPass, CSS2DRenderer) with the passes and shaders they import, and three.js's MIT licence: 14 files, 0.78 MiB. Its README records the version, the source URL, the tarball's integrity as the registry gives it, and each file's SHA-256, and how to upgrade. Nothing is edited. The minified core is used because nothing here reads it, and it halves the size.

**Nothing loads from outside the Platform.** The page loads no fonts (it uses the system's sans-serif), no CDN and no images. Two things hold it to that:

- The server sends a **Content-Security-Policy** with the web directory (`internal/argus/server.go`): scripts, styles, images, fonts and connections from Argus only, and the import map allowed by its SHA-256, which the server reads from `index.html` at start. So the browser refuses anything else, and the page writes no `style` attribute and no HTML strings (`js/ui/dom.js`); styles set from code go through the CSSOM, which the policy does not govern. The card's links are navigations, which it does not govern either.
- `TestThePageLoadsNothingFromOutside` walks the embedded directory: every file `index.html` names, every import map target and every module's imports must be in it, and the CSS must load nothing. A deliberately broken import (`https://cdn.example.test/x.js`) fails it.

The embedded page makes the binary 13.0 MiB (linux/amd64, `-s -w`), from #118's 12.1 MiB. `http.FileServerFS` sends no cache validators for embedded files, so a browser fetches the 0.9 MiB again on each visit; see the open questions.

## The stream, and when it stops

`js/stream.js` opens `/events` and hands each message to the model; a note also goes to the camera, unless it only reports something finished, was seeded, or is the seam. `EventSource` reconnects by itself after a dropped connection, and every reconnect gets a fresh snapshot (#118).

- **A dropped stream is a lost picture.** Once the stream has been down for 4 s, the page draws the last known state grey and dim under a banner, "Argus cannot be reached", exactly as for the `cluster` message's `lost`, and the camera holds back. Both come back on the next snapshot.
- **The Itema login session expires.** oauth2-proxy then answers the reconnect with a redirect, which `EventSource` cannot follow, and it gives up (`CLOSED`). The page then asks for `healthz` with `redirect: 'manual'`: an opaque redirect, or oauth2-proxy's 401, means the session has expired, and the page reloads, which does follow the redirect to sign in.
- **The stream keeps failing while Argus answers.** After 2 minutes of that the page reloads too.
- **Argus does not answer at all** (a network error): the page keeps the last known state on screen and keeps trying, backing off from 3 s to a minute. Reloading would only replace the picture with the browser's error page.
- **Reloads are bounded** (`reloadAllowed`): at most 5 in 30 minutes, and never sooner than a minute after the last, a gap that doubles with each recent one. The page remembers its reloads in `sessionStorage`, so the page it reloads into knows them.

## The drawing

The particle network is the prototype's: the core is ArgoCD, a dense warm cloud that breathes faster while it works; the other components sit on a ring round it, each joined to it; each Application's hub is on its slot's orbit with one filament to the core's edge; its Environments are clouds round the hub (prod, staging, then up to three Preview Environments, with "+n previews" beyond), and each Capability sits on its Environment. All of it is one point buffer and one line buffer refilled every frame, with the prototype's additive shaders, and the glow is UnrealBloom. Labels are HTML through CSS2DRenderer.

**Every state, with a shape as well as a colour.** Each loud look has a shape that reads in greyscale, and a tag with a mark and a border style of its own:

| State | The cloud | Its tag |
|---|---|---|
| Healthy | calm | none |
| Deploying, Updating | an amber wave runs through it | none |
| Arriving | cyan; its particles leave the core one by one along the filament and settle; a whole new Application also sends rings out | `+ NEW`, or `+ NEW · SETTING UP` for a new Application; none for a Preview Environment, whose arrival is quiet |
| Unreleased | a thin broken ring with nothing inside | `UNRELEASED`, dashed border |
| **Stuck** | orange, **frozen still behind a hard solid ring**, a point blinking above it | `■ STUCK`, thick solid border |
| **Degraded** | red, **broken into three pieces that pull apart**, links flickering within each piece only, throwing sparks | `✕ DEGRADED`, double border |
| **Unknown** | grey, **collapsed to a dotted outline** with the last known cloud faint inside | `? UNKNOWN`, dotted border |
| Leaving | greys, fades and disperses; a database's final backup travels home along the filament as blue beads; then it crumbles | `LEAVING` |
| A Capability's Warning | blinking amber on the Capability | `! BACKUPS`, `! ARCHIVING`, `! CERTIFICATE` or `! TASK FAILED` |
| A component | the same looks on its small cloud | the same tags |
| Cluster or stream lost | the whole scene drains to grey and dims (a CSS filter on the canvas and the labels) under a banner | |

The rings, outlines and pieces face the camera, so the shapes read from every angle. The screenshots below were also taken with the page in greyscale: the stuck ring, the Degraded pieces and the Unknown outline stay apart, and so do their tags.

**The Deploy beads** (#106), from `deploys.js`: each hop's bead travels its own leg of the route and holds at its end, and the route's waypoints are named (the gate, the core's edge, the hub, the cloud, the staging cloud), so the drawing only says where those are.

| Hop | The bead |
|---|---|
| Accepted | leaves the Deploy gate's cluster, which lights up, arcs over into the core's edge (1.6 s) |
| Waiting for ArgoCD | pulses at the core's edge, a ring breathing round it |
| Applying | travels the filament to the hub (2.5 s), and holds there pulsing through the migration |
| Rolling out | enters the cloud (1.5 s) and fades into its amber wave |
| Serving | one flash, a ring spreading through the cloud, if the page saw it arrive |
| Stuck | freezes orange at the end of its hop's leg, part-way into the cloud for a rollout, blinking |
| Refused | a red bead from the gate into the core, which fizzles out in sparks |
| Superseded | dims and merges into the core's edge, where the next one is |
| Promote | pale gold; at Accepted it lifts off the staging cloud, crosses the hub and the filament into the core, then follows the usual hops out to prod |

A bead is a hot white centre in a halo of its colour, so that it shows against the core's glow as well as against the dark: an amber bead on the warm core was invisible in the first screenshots. The core's edge sits a unit outside the core for the same reason.

**Labels do not pile up** (`labels.js`). Every 120 ms the page projects each label to the screen and shows them greedily by priority, hiding any that would overlap one already shown: a loud state's tag first, then what the pointer or the card is on, then other tags, Application names, component names, Environment names, and details such as a domain's host. The choice is stable from frame to frame. Component and Environment labels also fade out with distance, as in the prototype. With 30 Environments from the home view, 33 of 62 labels showed and none overlapped.

**Reduced motion.** With `prefers-reduced-motion: reduce`, the automatic camera starts off, particles drift, spin, shake and spark at a quarter of the speed, flights are cuts, and the circling is at most slow; the shapes and tags are the same.

## The camera

`attention.js` decides and the drawing flies. It is a pure object stepped once a frame with the clock, which is how the Node tests below run its rules. Every change the stream sends as a note has a place and a loudness; the server gives the loudness (#118's table), and the camera turns it into rank, lingering and expiry.

## The detail card

The card is panel B: a peek next to what the pointer is on, which replaces tooltips, pinned by a click. It sits beside its object, outside its radius on the screen, and follows it as the camera moves. `card.js` says what it shows, from the stream only.

- **Environment:** Condition and Activity, with why it is Degraded or stuck first; the hop strip, a Preview Environment's first two hops marked skipped; the peek's three facts (why, or the image, when it was deployed, the pods); and under "All details" the image and when it was deployed, pods and restarts, addresses, the last migration and the backups, Scheduled tasks and their last runs, ArgoCD's sync, health and last sync, the Capabilities with their state and Warning, and its latest feed entries.
- **Platform component:** what it does, its version and state, why when it is not Healthy, and its feed entries.
- **The core:** counts of Applications and Environments, what needs attention, what is deploying, and ArgoCD's version and state.
- **Links:** an Environment's address, ArgoCD, Grafana, and the last Deploy's commit in the Platform repository; a component's ArgoCD (not Traefik's or k3s's), Grafana and its template in `bootstrap/` at the pinned revision; the core's ArgoCD, Grafana and Platform repository. Only `http` and `https` addresses become links.
- **Not shown:** logs, environment variables, anything that acts.

Pinning flies to the object and holds the camera still; the chip says so, and loud and normal changes elsewhere get edge markers. Esc, the close button or a click on empty space closes it, which starts the usual 12 s pause.

## Backend additions

The card wanted links, when an image was deployed, and component versions, which #118's model did not carry (its open question "Links and versions"). Each addition is small and tested, and none adds a dependency or brings client-go outside `cmd/iidp-argus` (`TestOnlyArgusLinksClientGo` still passes).

- **Links.** The Deploy gate's `links` moved to `platformstate.LinksOf`, which the gate and Argus both call, so Argus's Environments carry iidp app status's `links`, ArgoCD and Grafana Explore by namespace. The snapshot gains `platform`: ArgoCD's and Grafana's addresses, the Platform repository's, and the bootstrap repository with its pinned revision, for the links that are not an Environment's. The bootstrap passes them in: `templates/argus.yaml` gives the component `links.*` from `platform.yaml`'s `argocdURL` and `grafanaURL`, `deployGate.platformRepository` and `bootstrap.repoURL`, and the Deployment sets them as `IIDP_ARGUS_*` variables, which `main` reads. `grafanaURL` is therefore now read by the bootstrap as well as the CLI; `values.yaml` lists it, empty, and an empty address leaves its links out. Tests: `TestLinksOf`, `TestNewPlatformGivesWebAddresses`, `TestSnapshotCarriesThePlatformsLinks`, and the two Argus bootstrap tests check the values and the environment.
- **`image.deployedAt`.** iidp app status reads it from the Platform repository's history, which Argus does not read. Argus has it from the Deploy that brought the tag: the newest Serving Deploy of that tag, joined by the Deploy gate's commit to the entry of ArgoCD's history that synced it, which is when it was deployed (else when the gate accepted it). The gate's Event lasts an hour, so the store keeps what it found for as long as the Environment runs that tag; a tag Argus never saw deployed, such as one from before it started, has none, and the card says "unknown: Argus did not see its Deploy". This is ArgoCD's sync time, where iidp app status gives the commit's time, a few seconds to minutes earlier. Test: `TestDeployedAtComesFromArgoCDsHistory`.
- **Component versions.** `platformstate.Component` gains `version`: k3s's is the node's kubelet version, and any other's the tag of the image named after the component (the image names hold `argocd`, `deploy-gate`, `cert-manager` and so on), among the images ArgoCD lists for it and its Deployments in view, or else the one tag all its images share. The Node's cut-down struct keeps `nodeInfo.kubeletVersion`, and the transform test says so. Tests: `TestComponentVersion`, `TestTransformsKeepOnlyWhatTheModelReads`.
- **The Content-Security-Policy** above, and its test `TestTheWebDirectoryHasAStrictContentSecurityPolicy`.

**Left out of the card**, because the cluster does not have them and Argus does not read the Platform repository:

- **The Application repository.** It is in the Platform repository's `applications/<name>/repository.yaml` binding; the image name is `ghcr.io/<owner>/<application>`, not the repository's.
- **The release of a Promote.** It is the Application repository's, as above. A Promote's link is its commit in the Platform repository, like a Deploy's.

## The demo

`cmd/iidp-argus/demo_test.go`'s `TestDemo` serves the page from the real store and server, fed a synthetic Platform the way the informers feed them, and changes it on a script. It runs only with `IIDP_ARGUS_DEMO` set to a listen address (the README's "Developing" section); `go test ./...` skips it.

- Scenarios: `tour` (the default) runs every animation round and round, a lost cluster every third round included; `still` shows every state and nothing moves; `thirty` has 15 Applications with prod and staging each, 30 Environments, and a Deploy every 4 s.
- GETs under `/demo/` make one change each, whatever the scenario: a Deploy through its hops (`?promote=1`, `?migrate=1`, `?fail=1`), a refusal, a superseded Deploy, crash-looping and back, a new Application, an Environment leaving, a burst, the API server going away and back, and the login session expiring (every other request then gets a 302 to a sign-in page, and the open streams end).
- `IIDP_ARGUS_DEMO_WEB` serves the web directory from disk, so an edit needs only a reload.

Everything in it goes through the real interpretation: a Deploy's hops come from `platformstate` reading the gate's Event, ArgoCD's sync and history, the migration Job and the pods, and the notes and their loudness from the store. So the page was checked against the real stream's shapes and timing.

## Verification

**Go.** `gofmt -l .` prints nothing; `go build ./...`, `go vet ./...` and `go test ./...` pass (the tests that listen on local ports need them); `goreleaser check` passes; `IIDP_REQUIRE_CHART_TOOLS=1 go test ./bootstrap/` passes with helm and kubeconform; `helm lint --strict bootstrap/components/argus --set bootstrapRevision=v0.0.0` passes.

**The logic modules' tests** run with Node, which CI does not install and this change does not add: `node --test cmd/iidp-argus/webtest`, 51 tests, all passing on Node 26. They drive the modules from outside with a clock the tests move: the camera's rules (17), the Deploy beads and the hop strip (8), the model's slots and times (6), the stream's reloads with a stand-in `EventSource` (7), the card and the looks (5), layout and labels (5), and the feed (3). The test files live outside `web`, so they are not embedded. The modules were also type-checked with TypeScript 5 and `@types/three@0.170.0` installed in a temporary directory (`checkJs`, `strict`): no errors. Neither is part of the build.

**In a browser.** Headless Chrome 153 with SwiftShader (`--use-angle=swiftshader --enable-unsafe-swiftshader`), driven over the DevTools protocol against the demo, at 1100×720:

- Every state of the `still` scenario was screenshotted from afar and close up, and the loud ones again in greyscale (a CSS filter on the page): the three loud shapes and their tags stay apart without colour.
- A Deploy with a migration was followed through its five hops with the card pinned: the hop strip read "accepted by the Deploy gate", "waiting for ArgoCD", "being applied", "rolling out", then the Deploy served and the card showed "Deployed just now" and its commit link. Screenshots from further out show the bead at each hop, the refused bead fizzling at the core, a Promote's bead on the filament from the hub, the superseded one, and the Serving flash.
- The card: the peek on hover, pinning, "All details", the links (address, ArgoCD with the Application's path, Grafana Explore by namespace), Esc.
- The session expiring: after `/demo/expire` the stream's reconnect met the redirect, the page logged "the Itema login session has expired; reloading the page", recorded the reload in `sessionStorage` and reloaded into the sign-in redirect.
- The console stayed empty through a load and a Deploy. The only other messages seen were SwiftShader's "GPU stall due to ReadPixels", which come with the screenshots, a 404 from a mistyped check, and, when the session expired, the Content-Security-Policy refusing the stream's redirect to the sign-in page, as it should.
- `TestThePageLoadsNothingFromOutside`, and the DevTools protocol's record of every request: none left `127.0.0.1` except the expired session's redirect and the reload into it.

**Frame rate.** The ticket asks for about 30 Environments at 30 fps or more on an ordinary laptop's integrated GPU. That could not be measured here: there was no GPU in reach, only Chrome's software renderer. `?fps=1` shows frames a second, the slowest frame in each second, and what was drawn. What was measured, on a MacBook Pro (Apple M5 Max) at 1100×720 and pixel ratio 1, glow included:

- With the `thirty` scenario (30 Environments, about 2,000 particles, 5,800 lines, 62 labels) the software renderer drew **29–51 fps**; with the `tour`, 25 fps while other work loaded the machine. It rasterises on the CPU, so these say little about a GPU.
- The page's own work each frame, rebuilding every particle and line, placing the labels and issuing the draw calls (`drawing.frame`, timed over 5 s), took **0.6 ms on average, 0.8 ms at worst**, with the 30 Environments. So the frame rate is bounded by rasterising, mostly the glow, not by the page's script, and there is room for a CPU several times slower.

**The maintainer should confirm the 30 fps on an ordinary laptop's integrated GPU** with the `thirty` demo and `?fps=1`, and record the laptop and the figure here. The page also looks after itself: after three seconds below 30 fps it drops a pixel ratio above 1 to 1, once.

### The camera, checked against the spec

Each rule of #103 and #115 "The camera", with how it was checked: **browser** is the demo in headless Chrome, reading the chip and the camera's mode after each change made through `/demo/`; **test** is a Node test in `webtest/attention.test.js` or `model.test.js`.

| Rule | Checked | Result |
|---|---|---|
| Idle, it hovers in a slow circle over the whole Platform | browser; test "idle, it goes home once" | chip "watching" |
| One entry per Application or the Platform; a newer or louder note replaces the older one | test "one entry per place" | pass |
| Visits go loudest first, then oldest | test "loudest first, then oldest" | pass |
| It flies there, circles, lingers 9, 7 or 4 s by loudness, then the next or back out | browser (a quiet Deploy visited, then back out); test "it lingers 9, 7 or 4 s" | pass |
| A louder note interrupts a quieter visit | browser (hello's quiet Deploy, then api staging Degraded: "showing api" 1.5 s later); test | pass |
| Quiet entries expire after 20 s unseen, normal after 60 s; loud wait until shown | test "quiet entries expire ..." | pass |
| It waits 0.7 s after the latest note | browser (0.4 s after: still "watching"; 1.6 s after: "showing hello"); test | pass |
| Three or more places within 3.7 s: one wide shot, interrupting any visit, then only the loud ones visited | browser (`/demo/burst`: "several changes at once", then "showing api", the loud one); test | pass |
| At most one wide shot every 8 s | test "at most one wide shot every 8 s" | pass |
| Cluster lost: back out to the whole Platform, circling slowly, visiting nothing until it returns; the banner says so | browser (`/demo/cluster`: after 33 s the banner, grey scene, chip "holding back: the cluster is lost"; a Deploy meanwhile not visited; visits resume when it returns); test | pass |
| A dropped stream looks the same, with its own words | browser (demo stopped: after 4 s the banner "Argus cannot be reached", grey scene, the chip says why) | pass |
| Only dragging, rotating or scrolling counts as someone using the map | code: the stage counts a drag past 4 px and the wheel; a click does not | by reading |
| While someone looks, the camera never moves on its own, and resumes 12 s after the last input | browser (a drag: "paused while you look around, back in 12 s", counting down; 12 s later it visited the waiting loud change); test | pass |
| Meanwhile loud and normal changes off screen get an edge marker: an arrow in the loudness's colour and the Application's name; a click flies there and counts as input | browser (timesheet prod Degraded during a drag: a red "timesheet" marker at the right edge); test | pass |
| Following a focus keeps it centred | code: a visit follows `where(key)` every frame | by reading; nothing moves in this drawing today |
| The feed: the latest six, newest at the bottom, a loudness dot and an age | browser (screenshots); test `feed.test.js` | pass |
| Clicking a feed entry flies there and counts as input | browser (a click: "paused while you look around") | pass |
| Hovering a feed entry rings its place | browser (screenshot with the ring) | pass |
| The chip says what the camera does and is the switch; on by default | browser (click: "off", click: on again and "showing hello"); test | pass |
| Notes that only report something finished go to the feed only | test "notes that only report something finished ..." | pass |
| Pinning a card flies to the object and holds the camera; the chip says so; closing starts the usual pause | browser ("paused while a card is open"; after Esc "paused while you look around, back in 12 s"); test | pass |
| Reduced motion: the automatic camera starts off | browser (emulated `prefers-reduced-motion: reduce`: chip "off") ; test | pass |

## Open questions

- **The frame rate on an ordinary laptop** is for the maintainer to measure and record here (above).
- **The Application repository and a Promote's release** are not linked (above). Either Argus reads the bindings, which means reading the Platform repository, or the CLI adds the repository to the Environment's ArgoCD Application as an annotation, which the model could then carry.
- **`deployedAt` after a restart.** A tag deployed more than an hour before Argus started has none until its next Deploy. Seeding it from ArgoCD's history would need the commit that set the tag, which only the Platform repository knows.
- **Component versions** are the image-name rule's best guess; a component with several images and none named after it (monitoring) has none. Its ArgoCD Application's chart version (`spec.source.targetRevision`) would be exact for the chart-installed components, at the cost of keeping one more field.
- **Caching.** Embedded files are served without validators, so each visit fetches the page's 0.9 MiB again. An `ETag` from the build's version would fix it; it did not seem worth it for an internal page opened now and then.
- **Crowding.** Two Applications on neighbouring slots can overlap on screen from some angles (#102's layout); labels are kept apart, but clouds are not moved, since nothing drawn may move.
