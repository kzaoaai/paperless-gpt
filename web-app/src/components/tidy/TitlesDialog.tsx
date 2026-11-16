import React, { useEffect, useMemo, useRef, useState } from "react";
import Button from "../ui/Button";
import Modal from "../ui/Modal";
import { TitleItem, TitleRun, TitleStatus, TitleView, tidyApi } from "./api";

interface TitlesDialogProps {
  writes: boolean;
  paperlessUrl: string;
  onClose: () => void;
  onDone: (message: string) => void;
}

// Roughly what one title prompt costs on Gemini 3 Flash at thinking level low.
const CENTS_PER_CALL = 0.3;
const TRY_FIRST = 20;
// paperless-ngx keeps titles to 128 characters.
const MAX_TITLE = 128;

/**
 * Words separated by spaces must all be in the row's current title or
 * suggestion; a comma separates alternatives. Several numbers alone are a list
 * of ids, and one number also matches the id.
 */
const titleMatches = (i: TitleItem, suggestion: string, query: string): boolean => {
  const q = query.trim().toLowerCase();
  if (!q) return true;
  const id = String(i.doc_id);
  const isNumber = (w: string) => /^#?\d+$/.test(w);
  const words = q.split(/[\s,]+/).filter(Boolean);
  if (words.length > 1 && words.every(isNumber)) return words.some((w) => w.replace("#", "") === id);
  const text = `${i.current}\n${suggestion}\n${i.error ?? ""}`.toLowerCase();
  const has = (w: string) => (isNumber(w) && w.replace("#", "") === id) || text.includes(w);
  return q.split(",").some((alt) => {
    const ws = alt.split(/\s+/).filter(Boolean);
    return ws.length > 0 && ws.every(has);
  });
};

/**
 * Re-check titles: paperless-gpt's own title prompt is asked about documents
 * whose title looks like an upload file name or an ID, in the background. The
 * page shows the current title beside the suggestion, which can be edited;
 * selected rows are applied as one logged action that undo reverses.
 */
const TitlesDialog: React.FC<TitlesDialogProps> = ({ writes, paperlessUrl, onClose, onDone }) => {
  const [run, setRun] = useState<TitleRun | null>(null);
  const [view, setView] = useState<TitleView | null>(null);
  const [status, setStatus] = useState<TitleStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [edited, setEdited] = useState<Record<number, string>>({});
  const [selected, setSelected] = useState<Record<number, boolean>>({});
  const lastPicked = useRef<number | null>(null);
  const [filter, setFilter] = useState("");
  const [showDone, setShowDone] = useState(false);
  const [showIgnored, setShowIgnored] = useState(false);

  const fail = (e: unknown) => setError(e instanceof Error ? e.message : String(e));

  useEffect(() => {
    tidyApi.titlesStatus().then(setStatus).catch(fail);
    tidyApi
      .titlesRuns()
      .then((runs) => {
        const latest = runs[0] ?? null;
        setRun(latest);
        if (latest && latest.status !== "running") return tidyApi.titlesView(latest.id).then(setView);
      })
      .catch(fail)
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    if (!run || run.status !== "running") return;
    const timer = setInterval(() => {
      tidyApi
        .titlesRuns()
        .then((runs) => {
          const r = runs.find((x) => x.id === run.id);
          if (!r) return;
          setRun(r);
          if (r.status !== "running") tidyApi.titlesView(r.id).then(setView);
        })
        .catch(() => {});
    }, 3000);
    return () => clearInterval(timer);
  }, [run]);

  const start = async (limit: number) => {
    setBusy(true);
    setError(null);
    try {
      setView(null);
      setEdited({});
      setSelected({});
      setFilter("");
      setRun(await tidyApi.titlesStart(limit));
    } catch (e) {
      fail(e);
    } finally {
      setBusy(false);
    }
  };

  const ignoredIds = useMemo(() => new Set((status?.ignored ?? []).map((x) => x.doc_id)), [status]);
  const items = useMemo(() => (view?.items ?? []).filter((i) => !ignoredIds.has(i.doc_id)), [view, ignoredIds]);
  const suggestion = (i: TitleItem) => edited[i.doc_id] ?? i.suggested;
  // Nothing left to decide: retitled or edited since, gone, or the suggestion is what it already says.
  const isDone = (i: TitleItem) => i.changed || i.gone || (!i.error && suggestion(i).trim() === i.current);
  const doneCount = items.filter(isDone).length;
  const open = showDone ? items : items.filter((i) => !isDone(i));
  const shown = open.filter((i) => titleMatches(i, suggestion(i), filter));
  const filtering = filter.trim() !== "";

  const pick = (docId: number, on: boolean, range: boolean) => {
    const ids = shown.map((i) => i.doc_id);
    const from = lastPicked.current === null ? -1 : ids.indexOf(lastPicked.current);
    const to = ids.indexOf(docId);
    const span = range && from >= 0 ? ids.slice(Math.min(from, to), Math.max(from, to) + 1) : [docId];
    setSelected({ ...selected, ...Object.fromEntries(span.map((id) => [id, on])) });
    lastPicked.current = docId;
  };
  const picked = open.filter((i) => selected[i.doc_id]);
  const pickedShown = shown.filter((i) => selected[i.doc_id]).length;
  const applicable = picked.filter((i) => !isDone(i) && !i.error && suggestion(i).trim() !== "");

  const apply = async () => {
    if (!view) return;
    setBusy(true);
    setError(null);
    try {
      const a = await tidyApi.titlesApply(
        view.run.id,
        applicable.map((i) => ({ doc_id: i.doc_id, title: suggestion(i).trim() }))
      );
      onDone(a.summary);
    } catch (e) {
      fail(e);
    } finally {
      setBusy(false);
    }
  };

  const ignore = async (docs: { doc_id: number; title: string }[]) => {
    setError(null);
    try {
      await tidyApi.titlesIgnore(docs);
      setStatus(await tidyApi.titlesStatus());
      setSelected({});
      lastPicked.current = null;
    } catch (e) {
      fail(e);
    }
  };
  const unignore = async (ids: number[]) => {
    setError(null);
    try {
      await tidyApi.titlesUnignore(ids);
      setStatus(await tidyApi.titlesStatus());
    } catch (e) {
      fail(e);
    }
  };

  const candidates = status?.candidates ?? 0;
  const cost = (calls: number) => ((calls * CENTS_PER_CALL) / 100).toFixed(2);
  const link = (id: number) => (paperlessUrl ? `${paperlessUrl}/documents/${id}/details` : undefined);
  const ignoredRows = status?.ignored ?? [];

  return (
    <Modal open onClose={busy ? () => {} : onClose} title="Re-check titles" size="focus">
      <div className="flex min-h-0 flex-1 flex-col">
        <div className="flex-1 space-y-4 overflow-y-auto px-6 py-4 text-sm">
          <p className="text-muted">
            paperless-gpt's own title prompt is asked about each document whose title looks like an upload file name, an
            ID or a scanner default, and nothing else about the document is asked or changed. Edit a suggestion by typing
            in it. Nothing changes until you select rows and apply them; the change is logged and can be undone. A title
            edited since the run is never overwritten. An ignored document keeps its title and is not asked about again.
          </p>
          {loading && <p className="text-muted">Loading…</p>}

          {!loading && (!run || run.status !== "running") && (
            <div className="flex flex-wrap items-center gap-3 rounded-md border border-line px-3 py-2">
              <span className="text-muted">
                {status ? `${candidates} of ${status.documents} titles look like a file name or an ID.` : "…"}
              </span>
              <Button size="sm" onClick={() => start(TRY_FIRST)} loading={busy} disabled={candidates === 0}>
                Try the newest {TRY_FIRST} (about ${cost(Math.min(candidates, TRY_FIRST))})
              </Button>
              <Button
                size="sm"
                variant={run ? "secondary" : "primary"}
                onClick={() => start(0)}
                loading={busy}
                disabled={candidates === 0}
              >
                Ask about all {candidates} (about ${cost(candidates)})
              </Button>
              {ignoredRows.length > 0 && (
                <Button size="sm" variant="ghost" onClick={() => setShowIgnored(!showIgnored)}>
                  {showIgnored ? "Hide" : "Show"} {ignoredRows.length} ignored
                </Button>
              )}
            </div>
          )}

          {run && run.status === "running" && (
            <div className="flex items-center justify-between gap-3 rounded-md border border-primary px-3 py-2">
              <span>
                Asking… {run.done} of {run.total} documents
              </span>
              <Button size="sm" variant="ghost" onClick={() => tidyApi.titlesStop().catch(() => {})}>
                Stop
              </Button>
            </div>
          )}

          {showIgnored && ignoredRows.length > 0 && (
            <div className="rounded-md border border-line">
              <p className="border-b border-line bg-surface-2 px-2 py-2 text-xs text-muted">
                Ignored: these keep their titles and are not asked about again.
              </p>
              <ul className="divide-y divide-line">
                {ignoredRows.map((d) => (
                  <li key={d.doc_id} className="flex items-center justify-between gap-3 px-2 py-1.5">
                    <span>
                      <span className="mr-2 tabular-nums text-faint">{d.doc_id}</span>
                      {d.gone ? <span className="text-faint">gone from paperless-ngx</span> : d.current}
                    </span>
                    <Button size="sm" variant="ghost" onClick={() => unignore([d.doc_id])}>
                      Un-ignore
                    </Button>
                  </li>
                ))}
              </ul>
            </div>
          )}

          {view && (
            <>
              <div className="space-y-2">
                <p className="text-xs text-muted">
                  Answers from the last run (run {view.run.id}, {new Date(view.run.created_at).toLocaleString()};{" "}
                  {view.run.status}, {view.run.done} of {view.run.total} documents). A new run replaces them.
                </p>
                <div className="flex flex-wrap items-center gap-2">
                  <input
                    type="search"
                    aria-label="Filter the documents"
                    title="Words must all be in the row; a comma separates alternatives. Several numbers are a list of ids."
                    className="w-72 rounded-md border border-line bg-surface px-2 py-1 text-sm"
                    placeholder="Filter: words, or ids like 12, 15"
                    value={filter}
                    onChange={(e) => setFilter(e.target.value)}
                  />
                  {filtering && (
                    <span className="text-xs text-muted">
                      {shown.length} of {open.length} shown
                    </span>
                  )}
                  <span className="ml-auto" />
                  {doneCount > 0 && (
                    <Button size="sm" variant="ghost" onClick={() => setShowDone(!showDone)}>
                      {showDone ? "Hide" : "Show"} {doneCount} done or unchanged
                    </Button>
                  )}
                </div>
              </div>
              <div className="overflow-x-auto rounded-md border border-line">
                <table className="w-full text-left text-sm">
                  <thead className="border-b border-line bg-surface-2 text-xs text-muted">
                    <tr>
                      <th className="w-8 px-2 py-2">
                        <input
                          type="checkbox"
                          aria-label="Select every row shown"
                          className="accent-[var(--color-primary)]"
                          disabled={shown.length === 0}
                          checked={shown.length > 0 && pickedShown === shown.length}
                          ref={(el) => {
                            if (el) el.indeterminate = pickedShown > 0 && pickedShown < shown.length;
                          }}
                          onChange={(e) =>
                            setSelected({
                              ...selected,
                              ...Object.fromEntries(shown.map((i) => [i.doc_id, e.target.checked])),
                            })
                          }
                        />
                      </th>
                      <th className="w-16 px-2 py-2 font-medium">ID</th>
                      <th className="w-2/5 px-2 py-2 font-medium">Current title</th>
                      <th className="px-2 py-2 font-medium">Suggested title</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-line">
                    {shown.length === 0 && (
                      <tr>
                        <td colSpan={4} className="px-2 py-3 text-muted">
                          {filtering ? "No document matches the filter." : "Nothing is left to decide in this run."}
                        </td>
                      </tr>
                    )}
                    {shown.map((i) => {
                      const s = suggestion(i);
                      return (
                        <tr key={i.doc_id} className={`align-top ${selected[i.doc_id] ? "bg-surface-2" : ""}`}>
                          <td className="px-2 py-1.5">
                            <input
                              type="checkbox"
                              aria-label={`Select ${i.current || `document ${i.doc_id}`}`}
                              title="Select this row. Shift-click selects the rows in between."
                              className="mt-1 accent-[var(--color-primary)]"
                              checked={!!selected[i.doc_id]}
                              onChange={() => {}}
                              onClick={(e) => pick(i.doc_id, !selected[i.doc_id], e.shiftKey)}
                            />
                          </td>
                          <td className="whitespace-nowrap px-2 py-1.5 tabular-nums">
                            {link(i.doc_id) ? (
                              <a className="text-primary hover:underline" href={link(i.doc_id)} target="_blank" rel="noreferrer">
                                {i.doc_id}
                              </a>
                            ) : (
                              i.doc_id
                            )}
                          </td>
                          <td className="px-2 py-1.5">
                            {i.gone ? <span className="text-faint">gone from paperless-ngx</span> : i.current}
                            {i.changed && !i.gone && (
                              <span className="block text-xs text-faint">changed since the run (was “{i.title}”)</span>
                            )}
                          </td>
                          <td className="px-2 py-1">
                            {i.error ? (
                              <span className="text-xs text-neg">{i.error}</span>
                            ) : (
                              <>
                                <input
                                  type="text"
                                  aria-label={`Suggested title for document ${i.doc_id}`}
                                  className="w-full rounded-md border border-line bg-surface px-2 py-1 text-sm"
                                  maxLength={MAX_TITLE}
                                  disabled={i.changed || i.gone}
                                  value={s}
                                  onChange={(e) => setEdited({ ...edited, [i.doc_id]: e.target.value })}
                                />
                                {edited[i.doc_id] !== undefined && edited[i.doc_id] !== i.suggested && (
                                  <button
                                    type="button"
                                    className="mt-0.5 text-xs text-faint hover:underline"
                                    onClick={() => {
                                      const next = { ...edited };
                                      delete next[i.doc_id];
                                      setEdited(next);
                                    }}
                                  >
                                    Back to the suggestion
                                  </button>
                                )}
                                {!i.changed && !i.gone && s.trim() === i.current && (
                                  <span className="block text-xs text-faint">same as now</span>
                                )}
                              </>
                            )}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            </>
          )}
          {!writes && <p className="text-warn">Read-only: set TIDY_WRITES=true to apply.</p>}
          {error && <p className="text-neg">{error}</p>}
        </div>
        <div className="flex items-center justify-end gap-2 border-t border-line px-6 py-4">
          {picked.length - pickedShown > 0 && (
            <span className="mr-auto text-xs text-warn">
              {picked.length - pickedShown} of the selected documents are hidden by the filter.
            </span>
          )}
          {picked.length > 0 && (
            <Button
              onClick={() => ignore(picked.map((i) => ({ doc_id: i.doc_id, title: i.current })))}
              disabled={busy}
            >
              Ignore {picked.length} selected
            </Button>
          )}
          <Button onClick={onClose} disabled={busy}>
            Close
          </Button>
          <Button variant="primary" onClick={apply} loading={busy} disabled={!writes || applicable.length === 0}>
            Apply to {applicable.length} selected
          </Button>
        </div>
      </div>
    </Modal>
  );
};

export default TitlesDialog;
