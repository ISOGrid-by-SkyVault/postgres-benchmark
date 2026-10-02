import { useCallback, useEffect, useMemo, useState } from "react";
import { api, type Info, type Result, type Run, type RunDetail, type RunParams } from "./api";
import { compare, formatCount, formatDate, formatMs, headline, topologyLabel } from "./format";

const SCALES = [1_000, 10_000, 50_000, 100_000];
const DURATIONS = [1, 3, 5, 10];
const POLL_MS = 1000;
const query = new URLSearchParams(window.location.search);

const CATEGORY_LABELS: Record<string, string> = {
  schema: "Schema",
  write: "Writes",
  read: "Reads",
  join: "Joins and complex queries",
  transaction: "Transactions",
  replication: "Replication",
};

export function App() {
  const [info, setInfo] = useState<Info | null>(null);
  const [runs, setRuns] = useState<Run[]>([]);
  // ?run=<id>&compare=<id> opens the page on a given run or comparison
  const [selectedId, setSelectedId] = useState<string | null>(() => query.get("run"));
  const [detail, setDetail] = useState<RunDetail | null>(null);
  const [baselineId, setBaselineId] = useState(() => query.get("compare") ?? "");
  const [baseline, setBaseline] = useState<RunDetail | null>(null);
  const [error, setError] = useState("");

  const refresh = useCallback(async () => {
    try {
      const [nextInfo, nextRuns] = await Promise.all([api.info(), api.runs()]);
      setInfo(nextInfo);
      setRuns(nextRuns);
      setSelectedId((current) => current ?? nextRuns[0]?.id ?? null);
      setError("");
    } catch (err) {
      setError(`Cannot reach the backend: ${(err as Error).message}`);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // Load the selected run, and keep reloading it while it is running.
  useEffect(() => {
    if (!selectedId) {
      setDetail(null);
      return;
    }
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;

    const load = async () => {
      try {
        const next = await api.run(selectedId);
        if (cancelled) return;
        setDetail(next);
        if (next.status === "running") {
          timer = setTimeout(load, POLL_MS);
        } else {
          void refresh(); // the list shows the final status
        }
      } catch (err) {
        if (!cancelled) setError((err as Error).message);
      }
    };
    void load();

    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [selectedId, refresh]);

  useEffect(() => {
    if (!baselineId) {
      setBaseline(null);
      return;
    }
    let cancelled = false;
    api
      .run(baselineId)
      .then((next) => !cancelled && setBaseline(next))
      .catch((err: Error) => !cancelled && setError(err.message));
    return () => {
      cancelled = true;
    };
  }, [baselineId]);

  const start = async (params: RunParams) => {
    try {
      const run = await api.start(params);
      setBaselineId("");
      setSelectedId(run.id);
      await refresh();
    } catch (err) {
      setError((err as Error).message);
    }
  };

  const remove = async (id: string) => {
    try {
      await api.remove(id);
      if (selectedId === id) setSelectedId(null);
      if (baselineId === id) setBaselineId("");
      await refresh();
    } catch (err) {
      setError((err as Error).message);
    }
  };

  const running = runs.some((run) => run.status === "running") || detail?.status === "running";
  const baselines = runs.filter((run) => run.status === "completed" && run.id !== selectedId);

  return (
    <div className="page">
      <header className="masthead">
        <div>
          <h1>PostgreSQL Benchmark</h1>
          <p className="muted">
            Measures schema changes, writes, reads, joins, transactions and replication on a single-node or multi-node
            PostgreSQL database.
          </p>
        </div>
        <TargetCard info={info} />
      </header>

      {error && (
        <p className="alert" role="alert">
          {error}
        </p>
      )}

      <div className="layout">
        <aside className="sidebar">
          <RunForm info={info} disabled={!info || running || Boolean(info.targetError)} onStart={start} />
          <RunList runs={runs} selectedId={selectedId} onSelect={setSelectedId} />
        </aside>

        <main>
          {detail ? (
            <RunView
              run={detail}
              baseline={baseline}
              baselines={baselines}
              baselineId={baselineId}
              onBaseline={setBaselineId}
              onDelete={() => remove(detail.id)}
            />
          ) : (
            <section className="card empty">
              <h2>No run yet</h2>
              <p className="muted">Start a benchmark to see what the database can do.</p>
              {info && <ActionCatalog info={info} />}
            </section>
          )}
        </main>
      </div>
    </div>
  );
}

function TargetCard({ info }: { info: Info | null }) {
  if (!info) return <div className="card target muted">Connecting…</div>;

  const { topology } = info;
  return (
    <div className="card target">
      <span className="label">Database under test</span>
      <strong>{info.targetLabel}</strong>
      {topology ? (
        <dl>
          <div>
            <dt>Topology</dt>
            <dd>{topology.mode === "single" ? "Single node" : `Multi-node, ${topology.nodeCount} nodes`}</dd>
          </div>
          <div>
            <dt>Version</dt>
            <dd>PostgreSQL {topology.serverVersion}</dd>
          </div>
          <div>
            <dt>Reads go to</dt>
            <dd>
              {topology.readEndpoints > 0
                ? `${topology.readEndpoints} read ${topology.readEndpoints === 1 ? "replica" : "replicas"}`
                : "the primary"}
            </dd>
          </div>
        </dl>
      ) : (
        <p className="alert">Not reachable: {info.targetError}</p>
      )}
    </div>
  );
}

function RunForm({
  info,
  disabled,
  onStart,
}: {
  info: Info | null;
  disabled: boolean;
  onStart: (params: RunParams) => void;
}) {
  const maxConcurrency = info?.maxConcurrency ?? 32;
  const [scale, setScale] = useState(10_000);
  const [concurrency, setConcurrency] = useState(8);
  const [durationSeconds, setDurationSeconds] = useState(3);

  return (
    <form
      className="card form"
      onSubmit={(event) => {
        event.preventDefault();
        onStart({ scale, concurrency: Math.min(concurrency, maxConcurrency), durationSeconds });
      }}
    >
      <h2>New run</h2>

      <label>
        Data size
        <select value={scale} onChange={(event) => setScale(Number(event.target.value))}>
          {SCALES.map((value) => (
            <option key={value} value={value}>
              {formatCount(value)} customers, {formatCount(value * 5)} orders
            </option>
          ))}
        </select>
      </label>

      <label>
        Concurrent workers
        <input
          type="number"
          min={1}
          max={maxConcurrency}
          value={concurrency}
          onChange={(event) => setConcurrency(Number(event.target.value))}
        />
      </label>

      <label>
        Time per action
        <select value={durationSeconds} onChange={(event) => setDurationSeconds(Number(event.target.value))}>
          {DURATIONS.map((value) => (
            <option key={value} value={value}>
              {value} {value === 1 ? "second" : "seconds"}
            </option>
          ))}
        </select>
      </label>

      <button type="submit" disabled={disabled}>
        {info?.running ? "A run is in progress" : "Start benchmark"}
      </button>
    </form>
  );
}

function RunList({
  runs,
  selectedId,
  onSelect,
}: {
  runs: Run[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  return (
    <section className="card">
      <h2>History</h2>
      {runs.length === 0 ? (
        <p className="muted">Runs are saved here.</p>
      ) : (
        <ul className="runs">
          {runs.map((run) => (
            <li key={run.id}>
              <button
                type="button"
                className={run.id === selectedId ? "run selected" : "run"}
                aria-current={run.id === selectedId}
                onClick={() => onSelect(run.id)}
              >
                <span className="run-top">
                  <span>{formatDate(run.startedAt)}</span>
                  <StatusBadge status={run.status} />
                </span>
                <span className="muted">
                  {topologyLabel(run)} · {formatCount(run.scale)} customers · {run.concurrency} workers
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

// Status is carried by a symbol and a word, never by color alone.
function StatusBadge({ status }: { status: Run["status"] }) {
  const symbol = { running: "●", completed: "✓", failed: "✕" }[status];
  return (
    <span className={`status status-${status}`}>
      <span aria-hidden="true">{symbol}</span> {status}
    </span>
  );
}

function RunView({
  run,
  baseline,
  baselines,
  baselineId,
  onBaseline,
  onDelete,
}: {
  run: RunDetail;
  baseline: RunDetail | null;
  baselines: Run[];
  baselineId: string;
  onBaseline: (id: string) => void;
  onDelete: () => void;
}) {
  const progress = run.totalActions > 0 ? Math.round((run.completedActions / run.totalActions) * 100) : 0;

  return (
    <section className="card">
      <div className="run-header">
        <div>
          <h2>
            {topologyLabel(run)} <StatusBadge status={run.status} />
          </h2>
          <p className="muted">
            {run.targetLabel} · PostgreSQL {run.serverVersion} · {formatDate(run.startedAt)}
          </p>
          <p className="muted">
            {formatCount(run.scale)} customers · {run.concurrency} workers · {run.durationSeconds}s per action
          </p>
        </div>
        {run.status !== "running" && (
          <button type="button" className="secondary" onClick={onDelete}>
            Delete run
          </button>
        )}
      </div>

      {run.status === "running" && (
        <div className="progress" role="status">
          <div className="progress-track">
            <div className="progress-fill" style={{ width: `${progress}%` }} />
          </div>
          <span>
            {run.currentAction || "Starting"} ({run.completedActions} of {run.totalActions} done)
          </span>
        </div>
      )}

      {run.status === "failed" && (
        <p className="alert" role="alert">
          The run failed: {run.error}
        </p>
      )}

      {run.status === "completed" && baselines.length > 0 && (
        <label className="compare">
          Compare with
          <select value={baselineId} onChange={(event) => onBaseline(event.target.value)}>
            <option value="">No comparison</option>
            {baselines.map((other) => (
              <option key={other.id} value={other.id}>
                {topologyLabel(other)} · {formatCount(other.scale)} customers · {formatDate(other.startedAt)}
              </option>
            ))}
          </select>
        </label>
      )}

      {run.results.length > 0 && <ResultsTable run={run} baseline={baseline} />}
    </section>
  );
}

function ResultsTable({ run, baseline }: { run: RunDetail; baseline: RunDetail | null }) {
  const baselineByAction = useMemo(
    () => new Map((baseline?.results ?? []).map((result) => [result.action, result])),
    [baseline],
  );

  // Keep the order the actions ran in, grouped under their category.
  const groups = useMemo(() => {
    const out: { category: string; results: Result[] }[] = [];
    for (const result of run.results) {
      const last = out[out.length - 1];
      if (last?.category === result.category) last.results.push(result);
      else out.push({ category: result.category, results: [result] });
    }
    return out;
  }, [run.results]);

  return (
    <>
      {baseline && (
        <div className="legend" aria-label="Legend">
          <span>
            <i className="swatch swatch-current" /> This run ({topologyLabel(run)})
          </span>
          <span>
            <i className="swatch swatch-baseline" /> Compared run ({topologyLabel(baseline)})
          </span>
        </div>
      )}

      <div className="table-scroll">
        <table>
          <thead>
            <tr>
              <th scope="col">Action</th>
              <th scope="col">Node</th>
              <th scope="col" className="num">
                Result
              </th>
              {baseline && <th scope="col">Comparison</th>}
              <th scope="col" className="num">
                Operations
              </th>
              <th scope="col" className="num">
                p50
              </th>
              <th scope="col" className="num">
                p95
              </th>
              <th scope="col" className="num">
                p99
              </th>
              <th scope="col" className="num">
                Errors
              </th>
            </tr>
          </thead>
          {groups.map((group) => (
            <tbody key={`${group.category}-${group.results[0].position}`}>
              <tr className="group">
                <th colSpan={baseline ? 9 : 8} scope="colgroup">
                  {CATEGORY_LABELS[group.category] ?? group.category}
                </th>
              </tr>
              {group.results.map((result) => (
                <ResultRow
                  key={result.position}
                  result={result}
                  comparing={Boolean(baseline)}
                  baseline={baselineByAction.get(result.action)}
                />
              ))}
            </tbody>
          ))}
        </table>
      </div>
    </>
  );
}

function ResultRow({ result, comparing, baseline }: { result: Result; comparing: boolean; baseline?: Result }) {
  const current = headline(result);
  const other = baseline ? headline(baseline) : null;
  const timed = result.kind === "timed";

  return (
    <tr>
      <th scope="row">{result.title}</th>
      <td>{result.node}</td>
      <td className="num strong">{current.text}</td>
      {comparing && <td>{other ? <ComparisonBars current={current} baseline={other} /> : <span className="muted">not in that run</span>}</td>}
      <td className="num">{formatCount(result.operations)}</td>
      <td className="num">{timed ? formatMs(result.latencyP50Ms) : "–"}</td>
      <td className="num">{timed ? formatMs(result.latencyP95Ms) : "–"}</td>
      <td className="num">{timed ? formatMs(result.latencyP99Ms) : "–"}</td>
      <td className="num" title={result.errorSample || undefined}>
        {formatCount(result.errors)}
      </td>
    </tr>
  );
}

// Two bars on a shared scale: the longer one fills the track. Whether longer
// is better depends on the metric, so the verdict is spelled out next to them.
function ComparisonBars({
  current,
  baseline,
}: {
  current: ReturnType<typeof headline>;
  baseline: ReturnType<typeof headline>;
}) {
  const scale = Math.max(current.value, baseline.value) || 1;
  const width = (value: number) => `${Math.max((value / scale) * 100, 1)}%`;

  return (
    <div className="bars" title={`This run: ${current.text}\nCompared run: ${baseline.text}`}>
      <div className="bars-track">
        <div className="bar bar-current" style={{ width: width(current.value) }} />
        <div className="bar bar-baseline" style={{ width: width(baseline.value) }} />
      </div>
      <span className="bars-label">
        {compare(current, baseline)}
        <span className="muted"> vs {baseline.text}</span>
      </span>
    </div>
  );
}

function ActionCatalog({ info }: { info: Info }) {
  return (
    <div className="catalog">
      <h3>What a run measures</h3>
      <ol>
        {info.actions.map((action) => (
          <li key={action.key}>
            <strong>{action.title}</strong>
            <span className="muted"> on the {action.node}. </span>
            {action.description}
          </li>
        ))}
      </ol>
    </div>
  );
}
