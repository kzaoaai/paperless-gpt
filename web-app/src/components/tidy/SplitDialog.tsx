import React, { useEffect, useState } from "react";
import Button from "../ui/Button";
import Modal from "../ui/Modal";
import { Row, SplitApplyGroup, SplitRun, SplitView, TidyError, tidyApi } from "./api";

interface SplitDialogProps {
  bucket: Row;
  /** All correspondents, for choosing another target. */
  rows: Row[];
  writes: boolean;
  paperlessUrl: string;
  onClose: () => void;
  onDone: (message: string) => void;
}

// Roughly what one correspondent prompt costs on Gemini 3 Flash at thinking
// level low (measured 2026-10-31 for the custom-field prompt, which is larger).
const CENTS_PER_DOC = 0.3;

/**
 * Splits a catch-all correspondent: paperless-gpt's own correspondent prompt is
 * asked about each of its documents in the background, the answers are grouped
 * by correspondent, and only ticked groups are applied (one logged action).
 */
const SplitDialog: React.FC<SplitDialogProps> = ({ bucket, rows, writes, paperlessUrl, onClose, onDone }) => {
  const [run, setRun] = useState<SplitRun | null>(null);
  const [view, setView] = useState<SplitView | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [picked, setPicked] = useState<Record<string, boolean>>({});
  const [choice, setChoice] = useState<Record<string, number>>({}); // 0 = create the suggested name

  // Find this correspondent's latest run, if any.
  useEffect(() => {
    tidyApi
      .splitRuns()
      .then((runs) => {
        const mine = runs.find((r) => r.correspondent_id === bucket.id) ?? null;
        setRun(mine);
        if (mine && mine.status !== "running") return tidyApi.splitView(mine.id).then(setView);
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
  }, [bucket.id]);

  // Follow a running run.
  useEffect(() => {
    if (!run || run.status !== "running") return;
    const timer = setInterval(() => {
      tidyApi
        .splitRuns()
        .then((runs) => {
          const r = runs.find((x) => x.id === run.id);
          if (!r) return;
          setRun(r);
          if (r.status !== "running") tidyApi.splitView(r.id).then(setView);
        })
        .catch(() => {});
    }, 3000);
    return () => clearInterval(timer);
  }, [run]);

  const start = async () => {
    setBusy(true);
    setError(null);
    try {
      setView(null);
      setRun(await tidyApi.splitStart(bucket.id));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const stop = () => tidyApi.splitStop().catch(() => {});

  const apply = async () => {
    if (!view) return;
    const groups: SplitApplyGroup[] = view.groups
      .filter((g) => picked[g.key] && !g.stay)
      .map((g) => {
        const target = choice[g.key] ?? g.target_id ?? 0;
        return {
          doc_ids: g.docs.filter((d) => !d.moved).map((d) => d.id),
          target_id: target,
          new_name: target === 0 ? g.target_name : "",
        };
      })
      .filter((g) => g.doc_ids.length > 0);
    setBusy(true);
    setError(null);
    try {
      const a = await tidyApi.splitApply(view.run.id, groups);
      onDone(a.summary);
    } catch (e) {
      setError(e instanceof TidyError || e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const others = rows.filter((r) => r.id !== bucket.id);
  const tickedDocs = (view?.groups ?? [])
    .filter((g) => picked[g.key] && !g.stay)
    .reduce((n, g) => n + g.docs.filter((d) => !d.moved).length, 0);
  const cost = ((bucket.doc_count * CENTS_PER_DOC) / 100).toFixed(2);
  const docLink = (id: number) => (paperlessUrl ? `${paperlessUrl}/documents/${id}/details` : undefined);

  return (
    <Modal open onClose={busy ? () => {} : onClose} title={`Split “${bucket.name}”`} size="focus">
      {/* Fills what the title leaves of the panel; "h-full" would be the whole panel and push the footer out of sight. */}
      <div className="flex min-h-0 flex-1 flex-col">
        <div className="flex-1 space-y-4 overflow-y-auto px-6 py-4 text-sm">
          <p className="text-muted">
            paperless-gpt's correspondent prompt is asked about each of the {bucket.doc_count} documents, the way document
            processing would. Nothing changes until you tick groups and apply them; the move is logged and can be undone.
          </p>
          {loading && <p className="text-muted">Loading…</p>}

          {!loading && (!run || run.status !== "running") && (
            <div className="flex flex-wrap items-center gap-3 rounded-md border border-line px-3 py-2">
              <span className="text-muted">“{bucket.name}” itself is left out of the choices.</span>
              <Button size="sm" variant={run ? "secondary" : "primary"} onClick={start} loading={busy}>
                {run ? "Ask again" : "Ask the LLM"} ({bucket.doc_count} calls, about ${cost})
              </Button>
            </div>
          )}

          {run && run.status === "running" && (
            <div className="flex items-center justify-between gap-3 rounded-md border border-primary px-3 py-2">
              <span>
                Asking… {run.done} of {run.total} documents
              </span>
              <Button size="sm" variant="ghost" onClick={stop}>
                Stop
              </Button>
            </div>
          )}

          {view && (
            <>
              <p className="text-xs text-muted">
                Run {view.run.id} from {new Date(view.run.created_at).toLocaleString()}: {view.run.status}, {view.run.done}{" "}
                of {view.run.total} documents.
                {view.failed.length > 0 && ` ${view.failed.length} could not be asked.`}
              </p>
              <ul className="space-y-2">
                {view.groups.map((g) => {
                  const open = g.docs.filter((d) => !d.moved);
                  const target = choice[g.key] ?? g.target_id ?? 0;
                  return (
                    <li key={g.key} className="rounded-md border border-line px-3 py-2">
                      <div className="flex flex-wrap items-center gap-2">
                        <input
                          type="checkbox"
                          className="accent-[var(--color-primary)]"
                          disabled={g.stay || open.length === 0}
                          checked={!!picked[g.key]}
                          onChange={(e) => setPicked({ ...picked, [g.key]: e.target.checked })}
                        />
                        {g.stay ? (
                          <span className="text-muted">Stays on “{bucket.name}”</span>
                        ) : (
                          <select
                            className="rounded-md border border-line bg-surface px-2 py-1"
                            value={target}
                            onChange={(e) => setChoice({ ...choice, [g.key]: Number(e.target.value) })}
                          >
                            {g.new && <option value={0}>New: {g.target_name}</option>}
                            {others.map((r) => (
                              <option key={r.id} value={r.id}>
                                {r.name}
                              </option>
                            ))}
                          </select>
                        )}
                        <span className="text-xs text-faint">
                          {g.docs.length} documents{open.length < g.docs.length && `, ${g.docs.length - open.length} already moved`}
                        </span>
                        {g.new && g.similar && target === 0 && (
                          <span className="text-xs text-warn">looks like existing “{g.similar}”</span>
                        )}
                      </div>
                      <details className="mt-1">
                        <summary className="cursor-pointer text-xs text-muted">documents</summary>
                        <ul className="mt-1 space-y-0.5 pl-6 text-xs">
                          {g.docs.map((d) => (
                            <li key={d.id} className={d.moved ? "text-faint line-through" : ""}>
                              {docLink(d.id) ? (
                                <a className="hover:underline" href={docLink(d.id)} target="_blank" rel="noreferrer">
                                  {d.title || `#${d.id}`}
                                </a>
                              ) : (
                                d.title || `#${d.id}`
                              )}
                              <span className="ml-1 text-faint">#{d.id}</span>
                            </li>
                          ))}
                        </ul>
                      </details>
                    </li>
                  );
                })}
              </ul>
              {view.failed.length > 0 && (
                <details className="text-xs text-muted">
                  <summary className="cursor-pointer">Could not be asked</summary>
                  <ul className="mt-1 pl-6">
                    {view.failed.map((f) => (
                      <li key={f.doc_id}>
                        #{f.doc_id} {f.title}: {f.error}
                      </li>
                    ))}
                  </ul>
                </details>
              )}
            </>
          )}
          {!writes && <p className="text-warn">Read-only: set TIDY_WRITES=true to apply.</p>}
          {error && <p className="text-neg">{error}</p>}
        </div>
        <div className="flex justify-end gap-2 border-t border-line px-6 py-4">
          <Button onClick={onClose} disabled={busy}>
            Close
          </Button>
          <Button variant="primary" onClick={apply} loading={busy} disabled={!writes || !view || tickedDocs === 0}>
            Move {tickedDocs} ticked documents
          </Button>
        </div>
      </div>
    </Modal>
  );
};

export default SplitDialog;
