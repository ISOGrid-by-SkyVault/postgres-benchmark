// Typed client for the Go backend. Every URL is same-origin: nginx (production)
// or the Vite dev server (development) forwards /api to the backend.

export interface Topology {
  mode: "single" | "multi-node";
  nodeCount: number;
  serverVersion: string;
  streamingReplicas: number;
  readEndpoints: number;
}

export interface ActionInfo {
  key: string;
  title: string;
  category: string;
  description: string;
  node: "primary" | "replica";
}

export interface Info {
  targetLabel: string;
  topology: Topology | null;
  targetError: string;
  running: boolean;
  maxConcurrency: number;
  limits: {
    minScale: number;
    maxScale: number;
    minDurationSeconds: number;
    maxDurationSeconds: number;
  };
  actions: ActionInfo[];
}

export interface Run {
  id: string;
  status: "running" | "completed" | "failed";
  targetLabel: string;
  topology: "single" | "multi-node";
  nodeCount: number;
  serverVersion: string;
  scale: number;
  concurrency: number;
  durationSeconds: number;
  currentAction: string;
  completedActions: number;
  totalActions: number;
  error: string;
  startedAt: string;
  finishedAt: string | null;
}

export interface Result {
  position: number;
  action: string;
  title: string;
  category: string;
  node: "primary" | "replica";
  /** "timed": many operations with a latency distribution. "once": a single operation. */
  kind: "timed" | "once";
  operations: number;
  errors: number;
  rows: number;
  durationMs: number;
  opsPerSecond: number;
  latencyAvgMs: number;
  latencyP50Ms: number;
  latencyP95Ms: number;
  latencyP99Ms: number;
  latencyMinMs: number;
  latencyMaxMs: number;
  errorSample: string;
}

export interface RunDetail extends Run {
  results: Result[];
}

export interface RunParams {
  scale: number;
  concurrency: number;
  durationSeconds: number;
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, init);
  if (response.status === 204) return undefined as T;

  const body = await response.json().catch(() => null);
  if (!response.ok) {
    throw new Error(body?.error ?? `The server answered ${response.status}`);
  }
  return body as T;
}

export const api = {
  info: () => request<Info>("/api/info"),
  runs: () => request<Run[]>("/api/runs"),
  run: (id: string) => request<RunDetail>(`/api/runs/${id}`),
  start: (params: RunParams) =>
    request<Run>("/api/runs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(params),
    }),
  remove: (id: string) => request<void>(`/api/runs/${id}`, { method: "DELETE" }),
};
