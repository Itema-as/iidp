// @ts-check
// Argus's placeholder: keeps the domain model from /events and shows it as
// text. The message shapes are documented in internal/argus/doc.go.

/** @type {{applications: Map<string, any>, components: Map<string, any>, feed: any[]}} */
const model = { applications: new Map(), components: new Map(), feed: [] };

const clusterEl = /** @type {HTMLElement} */ (document.getElementById("cluster"));
const feedEl = /** @type {HTMLElement} */ (document.getElementById("feed"));
const modelEl = /** @type {HTMLElement} */ (document.getElementById("model"));

function render() {
  modelEl.textContent = JSON.stringify(
    { applications: [...model.applications.values()], components: [...model.components.values()] },
    null,
    2,
  );
  feedEl.replaceChildren(
    ...model.feed.slice(-50).reverse().map((note) => {
      const li = document.createElement("li");
      li.className = note.seam ? "seam" : note.loudness;
      const where = note.place.application
        ? `${note.place.application}${note.place.environment ? " " + note.place.environment : ""}`
        : `Platform${note.place.component ? " / " + note.place.component : ""}`;
      li.textContent = `${new Date(note.at).toLocaleTimeString()}  ${where}  ${note.message}`;
      return li;
    }),
  );
}

/** @param {{state: string, since?: string}} cluster */
function showCluster(cluster) {
  clusterEl.className = cluster.state;
  clusterEl.textContent = cluster.since ? `${cluster.state} since ${new Date(cluster.since).toLocaleTimeString()}` : cluster.state;
}

const events = new EventSource("events");
events.addEventListener("snapshot", (e) => {
  const s = JSON.parse(e.data);
  model.applications = new Map(s.applications.map((/** @type {any} */ a) => [a.name, a]));
  model.components = new Map(s.components.map((/** @type {any} */ c) => [c.name, c]));
  model.feed = s.feed;
  showCluster(s.cluster);
  render();
});
events.addEventListener("application", (e) => {
  const a = JSON.parse(e.data);
  model.applications.set(a.name, a);
  render();
});
events.addEventListener("application-removed", (e) => {
  model.applications.delete(JSON.parse(e.data).name);
  render();
});
events.addEventListener("component", (e) => {
  const c = JSON.parse(e.data);
  model.components.set(c.name, c);
  render();
});
events.addEventListener("component-removed", (e) => {
  model.components.delete(JSON.parse(e.data).name);
  render();
});
events.addEventListener("note", (e) => {
  model.feed.push(JSON.parse(e.data));
  model.feed = model.feed.slice(-200);
  render();
});
events.addEventListener("cluster", (e) => showCluster(JSON.parse(e.data)));
events.onerror = () => showCluster({ state: "reconnecting" });
