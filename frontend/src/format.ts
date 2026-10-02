import type { Result, Run } from "./api";

const integer = new Intl.NumberFormat("en-US", { maximumFractionDigits: 0 });
const compact = new Intl.NumberFormat("en-US", { maximumSignificantDigits: 3 });
const dateTime = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });

export const formatCount = (value: number) => integer.format(value);

/** Milliseconds with a precision that suits the magnitude. */
export function formatMs(value: number): string {
  if (value >= 1000) return `${compact.format(value / 1000)} s`;
  if (value >= 100) return `${value.toFixed(0)} ms`;
  if (value >= 10) return `${value.toFixed(1)} ms`;
  return `${value.toFixed(2)} ms`;
}

export const formatDate = (iso: string) => dateTime.format(new Date(iso));

export function topologyLabel(run: Pick<Run, "topology" | "nodeCount">): string {
  return run.topology === "single" ? "Single node" : `Multi-node (${run.nodeCount} nodes)`;
}

/**
 * The one number that summarises a result and that comparisons are made on:
 *   - replication actions: median lag, lower is better
 *   - one-shot actions (DDL, replica catch-up): duration, lower is better
 *   - bulk load: rows per second, higher is better
 *   - every other timed action: operations per second, higher is better
 */
export interface Headline {
  value: number;
  text: string;
  higherIsBetter: boolean;
}

export function headline(result: Result): Headline {
  if (result.action === "replication_lag") {
    return { value: result.latencyP50Ms, text: `${formatMs(result.latencyP50Ms)} lag`, higherIsBetter: false };
  }
  if (result.action === "bulk_load") {
    return { value: result.opsPerSecond, text: `${formatCount(result.opsPerSecond)} rows/s`, higherIsBetter: true };
  }
  if (result.kind === "once") {
    return { value: result.durationMs, text: formatMs(result.durationMs), higherIsBetter: false };
  }
  return { value: result.opsPerSecond, text: `${formatCount(result.opsPerSecond)} ops/s`, higherIsBetter: true };
}

/** "1.4× faster" / "1.2× slower" / "same", comparing a result with a baseline. */
export function compare(current: Headline, baseline: Headline): string {
  if (current.value <= 0 || baseline.value <= 0) return "n/a";
  const ratio = current.higherIsBetter ? current.value / baseline.value : baseline.value / current.value;
  if (ratio >= 0.98 && ratio <= 1.02) return "same";
  return ratio > 1 ? `${ratio.toFixed(2)}× faster` : `${(1 / ratio).toFixed(2)}× slower`;
}
