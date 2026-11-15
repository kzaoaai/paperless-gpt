import React, { useEffect, useState } from "react";
import Button from "../ui/Button";
import Modal from "../ui/Modal";
import { FillApplyItem, FillRun, FillView, tidyApi } from "./api";

interface FillDialogProps {
  missingTags: number;
  missingTypes: number;
  writes: boolean;
  paperlessUrl: string;
  onClose: () => void;
  onDone: (message: string) => void;
}

// Roughly what one tag or type prompt costs on Gemini 3 Flash at thinking level low.
const CENTS_PER_CALL = 0.3;
const TRY_FIRST = 20;

/**
 * Fills missing tags and document types: paperless-gpt's own tag and type
 * prompts are asked about each document that lacks them, in the background, and
 * only ticked answers are applied (one logged action, one undo).
 */
const FillDialog: React.FC<FillDialogProps> = ({ missingTags, missingTypes, writes, paperlessUrl, onClose, onDone }) => {
  const [run, setRun] = useState<FillRun | null>(null);
  const [view, setView] = useState<FillView | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [tickTags, setTickTags] = useState<Record<number, boolean>>({});
  const [tickType, setTickType] = useState<Record<number, boolean>>({});

  useEffect(() => {
    tidyApi
      .fillRuns()
      .then((runs) => {
        const latest = runs[0] ?? null;
        setRun(latest);
        if (latest && latest.status !== "running") return tidyApi.fillView(latest.id).then(setView);
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    if (!run || run.status !== "running") return;
    const timer = setInterval(() => {
      tidyApi
        .fillRuns()
        .then((runs) => {
          const r = runs.find((x) => x.id === run.id);
          if (!r) return;
          setRun(r);
          if (r.status !== "running") tidyApi.fillView(r.id).then(setView);
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
      setTickTags({});
      setTickType({});
      setRun(await tidyApi.fillStart(limit));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const items = view?.items ?? [];
  const canTag = (i: (typeof items)[number]) => i.ask_tags && !i.has_tags_now && i.tag_ids.length > 0;
  const canType = (i: (typeof items)[number]) => i.ask_type && !i.has_type_now && !!i.type_id;
  const tickAll = (on: boolean) => {
    setTickTags(Object.fromEntries(items.filter(canTag).map((i) => [i.doc_id, on])));
    setTickType(Object.fromEntries(items.filter(canType).map((i) => [i.doc_id, on])));
  };
  const chosen: FillApplyItem[] = items
    .map((i) => ({
      doc_id: i.doc_id,
      tag_ids: tickTags[i.doc_id] && canTag(i) ? i.tag_ids : [],
      type_id: tickType[i.doc_id] && canType(i) ? i.type_id ?? 0 : 0,
    }))
    .filter((c) => c.tag_ids.length > 0 || c.type_id > 0);

  const apply = async () => {
    if (!view) return;
    setBusy(true);
    setError(null);
    try {
      const a = await tidyApi.fillApply(view.run.id, chosen);
      onDone(a.summary);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const allCalls = missingTags + missingTypes;
  const cost = (calls: number) => ((calls * CENTS_PER_CALL) / 100).toFixed(2);
  const link = (id: number) => (paperlessUrl ? `${paperlessUrl}/documents/${id}/details` : undefined);

  return (
    <Modal open onClose={busy ? () => {} : onClose} title="Fill missing tags and types" size="focus">
      <div className="flex h-full flex-col">
        <div className="flex-1 space-y-4 overflow-y-auto px-6 py-4 text-sm">
          <p className="text-muted">
            paperless-gpt's own tag and document-type prompts are asked about each document that has no tags or no type,
            and nothing else about the document is asked or changed. Documents waiting in paperless-gpt's queue are
            skipped. Nothing changes until you tick answers and apply them; the change is logged and can be undone.
          </p>
          {loading && <p className="text-muted">Loading…</p>}

          {!loading && (!run || run.status !== "running") && (
            <div className="flex flex-wrap items-center gap-3 rounded-md border border-line px-3 py-2">
              <span className="text-muted">
                {missingTags} without tags, {missingTypes} without a type.
              </span>
              <Button size="sm" onClick={() => start(TRY_FIRST)} loading={busy}>
                Try the newest {TRY_FIRST} (about ${cost(Math.min(allCalls, TRY_FIRST * 2))})
              </Button>
              <Button size="sm" variant={run ? "secondary" : "primary"} onClick={() => start(0)} loading={busy}>
                Ask about all (up to {allCalls} calls, about ${cost(allCalls)})
              </Button>
            </div>
          )}

          {run && run.status === "running" && (
            <div className="flex items-center justify-between gap-3 rounded-md border border-primary px-3 py-2">
              <span>
                Asking… {run.done} of {run.total} documents
              </span>
              <Button size="sm" variant="ghost" onClick={() => tidyApi.fillStop().catch(() => {})}>
                Stop
              </Button>
            </div>
          )}

          {view && (
            <>
              <div className="flex flex-wrap items-center justify-between gap-2">
                <p className="text-xs text-muted">
                  Run {view.run.id} from {new Date(view.run.created_at).toLocaleString()}: {view.run.status},{" "}
                  {view.run.done} of {view.run.total} documents.
                </p>
                <div className="flex gap-2">
                  <Button size="sm" variant="ghost" onClick={() => tickAll(true)}>
                    Tick all
                  </Button>
                  <Button size="sm" variant="ghost" onClick={() => tickAll(false)}>
                    Untick all
                  </Button>
                </div>
              </div>
              <div className="overflow-x-auto rounded-md border border-line">
                <table className="w-full text-left text-sm">
                  <thead className="border-b border-line bg-surface-2 text-xs text-muted">
                    <tr>
                      <th className="px-2 py-2 font-medium">Document</th>
                      <th className="px-2 py-2 font-medium">Tags</th>
                      <th className="px-2 py-2 font-medium">Type</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-line">
                    {items.map((i) => (
                      <tr key={i.doc_id} className="align-top">
                        <td className="px-2 py-1.5">
                          {link(i.doc_id) ? (
                            <a className="hover:underline" href={link(i.doc_id)} target="_blank" rel="noreferrer">
                              {i.title || `#${i.doc_id}`}
                            </a>
                          ) : (
                            i.title || `#${i.doc_id}`
                          )}
                          <span className="ml-1 text-xs text-faint">#{i.doc_id}</span>
                          {i.error && <span className="block text-xs text-neg">{i.error}</span>}
                        </td>
                        <td className="px-2 py-1.5">
                          {!i.ask_tags ? (
                            <span className="text-xs text-faint">has tags</span>
                          ) : i.has_tags_now ? (
                            <span className="text-xs text-faint">tagged since</span>
                          ) : (
                            <label className="flex items-start gap-2">
                              <input
                                type="checkbox"
                                className="mt-1 accent-[var(--color-primary)]"
                                disabled={!canTag(i)}
                                checked={!!tickTags[i.doc_id]}
                                onChange={(e) => setTickTags({ ...tickTags, [i.doc_id]: e.target.checked })}
                              />
                              <span className="flex flex-wrap gap-1">
                                {i.tag_names.map((t) => (
                                  <span key={t} className="rounded bg-surface-2 px-1.5 py-0.5 text-xs">
                                    {t}
                                  </span>
                                ))}
                                {i.unknown_tags.map((t) => (
                                  <span key={t} className="px-1 text-xs text-faint line-through" title="No tag has this name">
                                    {t}
                                  </span>
                                ))}
                                {i.tag_names.length === 0 && <span className="text-xs text-faint">none suggested</span>}
                              </span>
                            </label>
                          )}
                        </td>
                        <td className="px-2 py-1.5">
                          {!i.ask_type ? (
                            <span className="text-xs text-faint">has a type</span>
                          ) : i.has_type_now ? (
                            <span className="text-xs text-faint">typed since</span>
                          ) : (
                            <label className="flex items-start gap-2">
                              <input
                                type="checkbox"
                                className="mt-1 accent-[var(--color-primary)]"
                                disabled={!canType(i)}
                                checked={!!tickType[i.doc_id]}
                                onChange={(e) => setTickType({ ...tickType, [i.doc_id]: e.target.checked })}
                              />
                              <span className={i.type_id ? "" : "text-xs text-faint line-through"}>
                                {i.type_name || "none suggested"}
                              </span>
                            </label>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </>
          )}
          {!writes && <p className="text-warn">Read-only: set TIDY_WRITES=true to apply.</p>}
          {error && <p className="text-neg">{error}</p>}
        </div>
        <div className="flex justify-end gap-2 border-t border-line px-6 py-4">
          <Button onClick={onClose} disabled={busy}>
            Close
          </Button>
          <Button variant="primary" onClick={apply} loading={busy} disabled={!writes || chosen.length === 0}>
            Apply {chosen.length} ticked documents
          </Button>
        </div>
      </div>
    </Modal>
  );
};

export default FillDialog;
