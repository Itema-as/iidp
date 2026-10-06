// @ts-check
// The shapes the stream carries (internal/argus/doc.go), for JSDoc. This
// module exports nothing at run time.

/**
 * @typedef {'Healthy' | 'Degraded' | 'Unknown'} ConditionState
 * @typedef {{state: ConditionState, reason?: string, warning?: string}} Condition
 * @typedef {'Accepted' | 'WaitingForArgoCD' | 'Applying' | 'RollingOut' | 'Serving'} Hop
 * @typedef {{
 *   tag: string, commit?: string, promote?: boolean, preview?: boolean, at?: string,
 *   hop?: Hop, stuck?: boolean, reason?: string, refused?: boolean, supersededBy?: string
 * }} Deploy
 * @typedef {'Arriving' | 'Unreleased' | 'Deploying' | 'Updating' | 'Leaving'} ActivityState
 * @typedef {{state: ActivityState, stuck: boolean, reason?: string, deploy?: Deploy}} Activity
 * @typedef {'postgres' | 'itema-login' | 'custom-domain' | 'scheduled-task'} CapabilityType
 * @typedef {{type: CapabilityType, name: string, condition: Condition, activity: Activity | null}} Capability
 * @typedef {{result: 'succeeded' | 'failed' | 'running' | 'pending', startedAt?: string, finishedAt?: string}} Run
 * @typedef {{name: string, schedule: string, lastScheduleTime?: string, lastRun: Run | null}} Task
 * @typedef {{phase: string, message?: string, startedAt?: string, finishedAt?: string}} Operation
 * @typedef {{application: string, sync: string, health: string, operation: Operation | null}} ArgoCDState
 * @typedef {{
 *   name: string, namespace?: string, argocd: ArgoCDState | null,
 *   image: {repository: string, tag: string, deployedAt?: string} | null,
 *   pods: {ready: number, total: number, restarts: number},
 *   migration: Run | null, tasks: Task[], addresses: string[],
 *   links?: {argocd?: string, grafana?: string},
 *   condition?: Condition, activity: Activity | null,
 *   databaseAccess?: {readWrite: string, readOnly: string, readWriteSetUp: boolean, readOnlySetUp: boolean},
 *   capabilities: Capability[], deploys: Deploy[]
 * }} Environment
 * @typedef {{name: string, environments: Environment[]}} Application
 * @typedef {{name: string, version?: string, condition: Condition, activity: Activity | null}} Component
 * @typedef {{application?: string, environment?: string, component?: string}} Place
 * @typedef {'loud' | 'normal' | 'quiet'} Loudness
 * @typedef {{
 *   id: number, at: string, place: Place, loudness: Loudness, message: string,
 *   feedOnly?: boolean, seeded?: boolean, seam?: boolean
 * }} Note
 * @typedef {{state: 'connected' | 'interrupted' | 'lost', since?: string}} ClusterState
 * @typedef {{
 *   argocdURL?: string, grafanaURL?: string, platformRepository?: string,
 *   bootstrapRepository?: string, bootstrapRevision?: string
 * }} PlatformLinks
 * @typedef {{
 *   at: string, restartedAt: string, ready: boolean, cluster: ClusterState,
 *   platform?: PlatformLinks, applications: Application[], components: Component[], feed: Note[]
 * }} Snapshot
 */

/**
 * What the pointer is on, and what a card is about.
 * @typedef {{kind: 'env', app: string, env: string, cap?: string}
 *   | {kind: 'component', name: string}
 *   | {kind: 'core'}} Target
 */

/** @typedef {[number, number, number]} Vec */

export {};
