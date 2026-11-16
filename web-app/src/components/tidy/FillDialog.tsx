import React, { useEffect, useMemo, useRef, useState } from "react";
import Button from "../ui/Button";
import Modal from "../ui/Modal";
import { FillApplyItem, FillItem, FillRun, FillStatus, FillView, tidyApi } from "./api";

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

type SortKey = "id" | "title" | "tags" | "type";

interface CellText {
  /** 0 a suggestion, 1 nothing suggested, 2 filled since the run, 3 never asked. */
  rank: number;
  text: string;
}

/** What a row's Tags cell says: the order sorting uses and the words the filter matches. */
const tagsCell = (i: FillItem): CellText => {
  if (!i.ask_tags) return { rank: 3, text: "has tags" };
  if (i.has_tags_now) return { rank: 2, text: "tagged since" };
  const names = [...i.tags.map((t) => t.name), ...i.unknown_tags].join(", ");
  return i.tags.length > 0 ? { rank: 0, text: names } : { rank: 1, text: `${names} none suggested`.trim() };
};

/** The same for the Type cell. */
const typeCell = (i: FillItem): CellText => {
  if (!i.ask_type) return { rank: 3, text: "has a type" };
  if (i.has_type_now) return { rank: 2, text: "typed since" };
  return i.type_id ? { rank: 0, text: i.type_name ?? "" } : { rank: 1, text: i.type_name || "none suggested" };
};

/** Nothing left to decide: everything the run asked about has been filled since. */
const isFilled = (i: FillItem) => (!i.ask_tags || i.has_tags_now) && (!i.ask_type || i.has_type_now);

const sortCell = (i: FillItem, key: SortKey): CellText =>
  key === "tags" ? tagsCell(i) : key === "type" ? typeCell(i) : { rank: i.title ? 0 : 1, text: i.title };

/**
 * Whether a row passes the filter. Words separated by spaces must all be in
 * the row's title, tags, type or error; a comma separates alternatives. One
 * number also matches the document's id exactly ("#12" matches only the id),
 * and several numbers alone are a list of ids, whatever separates them.
 */
const fillMatches = (i: FillItem, query: string): boolean => {
  const q = query.trim().toLowerCase();
  if (!q) return true;
  const id = String(i.doc_id);
  const isNumber = (w: string) => /^#?\d+$/.test(w);
  const words = q.split(/[\s,]+/).filter(Boolean);
  if (words.length > 1 && words.every(isNumber)) return words.some((w) => w.replace("#", "") === id);
  const text = [i.title, tagsCell(i).text, typeCell(i).text, i.error ?? ""].join("\n").toLowerCase();
  const has = (w: string) =>
    w.startsWith("#") && isNumber(w) ? w.slice(1) === id : (isNumber(w) && w === id) || text.includes(w);
  return q.split(",").some((alternative) => {
    const ws = alternative.split(/\s+/).filter(Boolean);
    return ws.length > 0 && ws.every(has);
  });
};

/**
 * Fills missing tags and document types: paperless-gpt's own tag and type
 * prompts are asked about each document that lacks them, in the background, and
 * only selected rows are applied (one logged action, one undo). A suggested
 * tag can be left out by clicking it, and a document can be put on an ignore
 * list so later runs skip it. The table sorts by any column and filters by
 * words or by a list of ids; the heading's box and the bulk ignore act on the rows shown.
 * The dialog opens on the last run's answers, labelled as such, and leaves out
 * the rows that have been filled since unless asked to show them. An ignored
 * document leaves the table at once; "Show N ignored" lists them all.
 */
const FillDialog: React.FC<FillDialogProps> = ({ missingTags, missingTypes, writes, paperlessUrl, onClose, onDone }) => {
  const [run, setRun] = useState<FillRun | null>(null);
  const [view, setView] = useState<FillView | null>(null);
  const [status, setStatus] = useState<FillStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Types left out by a click, the way a suggested tag is.
  const [droppedType, setDroppedType] = useState<Record<number, boolean>>({});
  const [dropped, setDropped] = useState<Record<number, number[]>>({});
  const [showIgnored, setShowIgnored] = useState(false);
  const [filter, setFilter] = useState("");
  const [showFilled, setShowFilled] = useState(false);
  // Rows picked with the first column's box: what Apply fills and what the bulk Ignore ignores.
  const [selected, setSelected] = useState<Record<number, boolean>>({});
  const lastPicked = useRef<number | null>(null);
  // A run lists the newest documents first, which is the highest id first.
  const [sort, setSort] = useState<{ key: SortKey; desc: boolean }>({ key: "id", desc: true });

  const fail = (e: unknown) => setError(e instanceof Error ? e.message : String(e));

  useEffect(() => {
    tidyApi.fillStatus().then(setStatus).catch(fail);
    tidyApi
      .fillRuns()
      .then((runs) => {
        const latest = runs[0] ?? null;
        setRun(latest);
        if (latest && latest.status !== "running") return tidyApi.fillView(latest.id).then(setView);
      })
      .catch(fail)
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
      setDroppedType({});
      setDropped({});
      setFilter("");
      setSelected({});
      setRun(await tidyApi.fillStart(limit));
    } catch (e) {
      fail(e);
    } finally {
      setBusy(false);
    }
  };

  const items = useMemo(() => view?.items ?? [], [view]);
  // The ignore list is the truth once it has loaded; before that, the run's own flag.
  const ignoredIds = useMemo(() => (status ? new Set(status.ignored.map((x) => x.doc_id)) : null), [status]);
  // The run's rows without the ignored ones, which are listed under "Show N ignored" instead.
  const kept = useMemo(
    () => items.filter((i) => !(ignoredIds ? ignoredIds.has(i.doc_id) : i.ignored)),
    [items, ignoredIds]
  );
  const filledCount = useMemo(() => kept.filter(isFilled).length, [kept]);
  // The rows there is still something to decide about, plus the filled ones when asked for.
  const open = useMemo(() => (showFilled ? kept : kept.filter((i) => !isFilled(i))), [kept, showFilled]);
  const shown = useMemo(() => {
    const list = open.filter((i) => fillMatches(i, filter));
    const dir = sort.desc ? -1 : 1;
    return [...list].sort((a, b) => {
      if (sort.key === "id") return (a.doc_id - b.doc_id) * dir;
      const ca = sortCell(a, sort.key);
      const cb = sortCell(b, sort.key);
      const byCell = ca.rank - cb.rank || ca.text.localeCompare(cb.text, undefined, { sensitivity: "base", numeric: true });
      // Rows that say the same keep the newest first, in either direction.
      return byCell * dir || b.doc_id - a.doc_id;
    });
  }, [open, filter, sort]);
  const filtering = filter.trim() !== "";
  const isIgnored = (i: FillItem) => (ignoredIds ? ignoredIds.has(i.doc_id) : i.ignored);
  const keptTags = (i: FillItem) => i.tags.filter((t) => !(dropped[i.doc_id] ?? []).includes(t.id));
  const canTag = (i: FillItem) => i.ask_tags && !i.has_tags_now && !isIgnored(i) && keptTags(i).length > 0;
  const canType = (i: FillItem) => i.ask_type && !i.has_type_now && !isIgnored(i) && !!i.type_id;
  // Still missing something, and the model had nothing usable to offer.
  const nothingSuggested = (i: FillItem) =>
    !isIgnored(i) &&
    !i.error &&
    ((i.ask_tags && !i.has_tags_now) || (i.ask_type && !i.has_type_now)) &&
    !(i.ask_tags && !i.has_tags_now && i.tags.length > 0) &&
    !(i.ask_type && !i.has_type_now && !!i.type_id);
  const empty = shown.filter(nothingSuggested);

  const toggleTag = (docId: number, tagId: number) => {
    const now = dropped[docId] ?? [];
    setDropped({ ...dropped, [docId]: now.includes(tagId) ? now.filter((t) => t !== tagId) : [...now, tagId] });
  };
  const chosen: FillApplyItem[] = items
    .map((i) => ({
      doc_id: i.doc_id,
      tag_ids: selected[i.doc_id] && canTag(i) ? keptTags(i).map((t) => t.id) : [],
      type_id: selected[i.doc_id] && canType(i) && !droppedType[i.doc_id] ? i.type_id ?? 0 : 0,
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
      fail(e);
    } finally {
      setBusy(false);
    }
  };

  const ignore = async (docs: { doc_id: number; title: string }[]) => {
    setError(null);
    try {
      await tidyApi.fillIgnore(docs);
      setStatus(await tidyApi.fillStatus());
    } catch (e) {
      fail(e);
    }
  };
  // Selects one row, or with Shift held every shown row from the last one picked to this one.
  const pick = (docId: number, on: boolean, range: boolean) => {
    const ids = shown.map((i) => i.doc_id);
    const from = lastPicked.current === null ? -1 : ids.indexOf(lastPicked.current);
    const to = ids.indexOf(docId);
    const span = range && from >= 0 ? ids.slice(Math.min(from, to), Math.max(from, to) + 1) : [docId];
    setSelected({ ...selected, ...Object.fromEntries(span.map((id) => [id, on])) });
    lastPicked.current = docId;
  };
  const pickShown = (on: boolean) =>
    setSelected({ ...selected, ...Object.fromEntries(shown.map((i) => [i.doc_id, on])) });
  // Selected rows that are still in the table: ignoring or filling one takes it out.
  const picked = open.filter((i) => selected[i.doc_id]);
  const pickedShown = shown.filter((i) => selected[i.doc_id]).length;
  const hiddenPicked = picked.length - pickedShown;
  const ignorePicked = async () => {
    await ignore(picked.map((i) => ({ doc_id: i.doc_id, title: i.title })));
    setSelected({});
    lastPicked.current = null;
  };
  const unignore = async (ids: number[]) => {
    setError(null);
    try {
      await tidyApi.fillUnignore(ids);
      setStatus(await tidyApi.fillStatus());
    } catch (e) {
      fail(e);
    }
  };

  const allCalls = status ? status.calls : missingTags + missingTypes;
  const cost = (calls: number) => ((calls * CENTS_PER_CALL) / 100).toFixed(2);
  const link = (id: number) => (paperlessUrl ? `${paperlessUrl}/documents/${id}/details` : undefined);
  const docTitle = (id: number, title: string) =>
    link(id) ? (
      <a className="hover:underline" href={link(id)} target="_blank" rel="noreferrer">
        {title || `#${id}`}
      </a>
    ) : (
      title || `#${id}`
    );
  const ignoredCount = status?.ignored.length ?? 0;
  const header = (key: SortKey, label: string, className = "") => (
    <th
      className={`px-2 py-2 font-medium ${className}`}
      aria-sort={sort.key === key ? (sort.desc ? "descending" : "ascending") : "none"}
    >
      <button
        type="button"
        className="hover:text-ink"
        title={`Sort by ${label.toLowerCase()}`}
        onClick={() => setSort((s) => ({ key, desc: s.key === key ? !s.desc : key === "id" }))}
      >
        {label}
        {sort.key === key ? (sort.desc ? " ↓" : " ↑") : ""}
      </button>
    </th>
  );

  return (
    <Modal open onClose={busy ? () => {} : onClose} title="Fill missing tags and types" size="focus">
      {/* Fills what the title leaves of the panel; "h-full" would be the whole panel and push the footer out of sight. */}
      <div className="flex min-h-0 flex-1 flex-col">
        <div className="flex-1 space-y-4 overflow-y-auto px-6 py-4 text-sm">
          <p className="text-muted">
            paperless-gpt's own tag and document-type prompts are asked about each document that has no tags or no type,
            and nothing else about the document is asked or changed. Documents waiting in paperless-gpt's queue are
            skipped. Nothing changes until you select rows and apply them; the change is logged and can be undone.
            The first column selects rows, for Apply or for Ignore; Shift-click selects a range. Click a suggested tag
            or type to leave it out. An ignored document leaves the list and is not asked about again; "Show ignored"
            lists them, with Un-ignore.
          </p>
          {loading && <p className="text-muted">Loading…</p>}

          {!loading && (!run || run.status !== "running") && (
            <div className="flex flex-wrap items-center gap-3 rounded-md border border-line px-3 py-2">
              <span className="text-muted">
                {status
                  ? `${status.documents} to ask about: ${status.without_tags} without tags, ${status.without_type} without a type.`
                  : `${missingTags} without tags, ${missingTypes} without a type.`}
              </span>
              <Button size="sm" onClick={() => start(TRY_FIRST)} loading={busy} disabled={allCalls === 0}>
                Try the newest {TRY_FIRST} (about ${cost(Math.min(allCalls, TRY_FIRST * 2))})
              </Button>
              <Button
                size="sm"
                variant={run ? "secondary" : "primary"}
                onClick={() => start(0)}
                loading={busy}
                disabled={allCalls === 0}
              >
                Ask about all (up to {allCalls} calls, about ${cost(allCalls)})
              </Button>
              {ignoredCount > 0 && (
                <Button size="sm" variant="ghost" onClick={() => setShowIgnored(!showIgnored)}>
                  {showIgnored ? "Hide" : "Show"} {ignoredCount} ignored
                </Button>
              )}
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

          {showIgnored && status && ignoredCount > 0 && (
            <div className="rounded-md border border-line">
              <p className="border-b border-line bg-surface-2 px-2 py-2 text-xs text-muted">
                Ignored documents: Fill does not ask about these. Nothing about them is changed in paperless-ngx.
              </p>
              <ul className="divide-y divide-line">
                {status.ignored.map((d) => (
                  <li key={d.doc_id} className="flex items-center justify-between gap-3 px-2 py-1.5">
                    <span>
                      {docTitle(d.doc_id, d.title)}
                      <span className="ml-1 text-xs text-faint">
                        #{d.doc_id} · ignored {new Date(d.created_at).toLocaleDateString()}
                      </span>
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
                  {filledCount > 0 && (
                    <Button size="sm" variant="ghost" onClick={() => setShowFilled(!showFilled)}>
                      {showFilled ? "Hide" : "Show"} {filledCount} already filled
                    </Button>
                  )}
                  {empty.length > 0 && (
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => ignore(empty.map((i) => ({ doc_id: i.doc_id, title: i.title })))}
                    >
                      Ignore the {empty.length}
                      {filtering ? " shown" : ""} with nothing suggested
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
                          title="Select every row shown"
                          className="accent-[var(--color-primary)]"
                          disabled={shown.length === 0}
                          checked={shown.length > 0 && pickedShown === shown.length}
                          ref={(el) => {
                            if (el) el.indeterminate = pickedShown > 0 && pickedShown < shown.length;
                          }}
                          onChange={(e) => pickShown(e.target.checked)}
                        />
                      </th>
                      {header("id", "ID", "w-16")}
                      {header("title", "Document")}
                      {header("tags", "Tags")}
                      {header("type", "Type")}
                      <th className="px-2 py-2 font-medium" />
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-line">
                    {shown.length === 0 && (
                      <tr>
                        <td colSpan={6} className="px-2 py-3 text-muted">
                          {filtering
                            ? "No document matches the filter."
                            : "Nothing is left of this run: its documents have been filled or ignored."}
                        </td>
                      </tr>
                    )}
                    {shown.map((i) => {
                      const cell = "px-2 py-1.5";
                      return (
                        <tr key={i.doc_id} className={`align-top ${selected[i.doc_id] ? "bg-surface-2" : ""}`}>
                          <td className="px-2 py-1.5">
                            <input
                              type="checkbox"
                              aria-label={`Select ${i.title || `document ${i.doc_id}`}`}
                              title="Select this row. Shift-click selects the rows in between."
                              className="mt-1 accent-[var(--color-primary)]"
                              checked={!!selected[i.doc_id]}
                              onChange={() => {}}
                              onClick={(e) => pick(i.doc_id, !selected[i.doc_id], e.shiftKey)}
                            />
                          </td>
                          <td className={`${cell} whitespace-nowrap tabular-nums`}>
                            {link(i.doc_id) ? (
                              <a className="hover:underline" href={link(i.doc_id)} target="_blank" rel="noreferrer">
                                {i.doc_id}
                              </a>
                            ) : (
                              i.doc_id
                            )}
                          </td>
                          <td className={cell}>
                            {i.title ? docTitle(i.doc_id, i.title) : <span className="text-faint">untitled</span>}
                            {i.error && <span className="block text-xs text-neg">{i.error}</span>}
                          </td>
                          <td className={cell}>
                            {!i.ask_tags ? (
                              <span className="text-xs text-faint">has tags</span>
                            ) : i.has_tags_now ? (
                              <span className="text-xs text-faint">tagged since</span>
                            ) : (
                              <div className="flex items-start gap-2">
                                <span className="flex flex-wrap gap-1">
                                  {i.tags.map((t) => {
                                    const out = (dropped[i.doc_id] ?? []).includes(t.id);
                                    return (
                                      <button
                                        key={t.id}
                                        type="button"
                                        aria-pressed={!out}
                                        title={out ? "Left out. Click to put it back." : "Click to leave this tag out."}
                                        onClick={() => toggleTag(i.doc_id, t.id)}
                                        className={`rounded px-1.5 py-0.5 text-xs ${
                                          out
                                            ? "border border-dashed border-line text-faint line-through"
                                            : "bg-surface-2 hover:bg-line"
                                        }`}
                                      >
                                        {t.name}
                                      </button>
                                    );
                                  })}
                                  {i.unknown_tags.map((t) => (
                                    <span key={t} className="px-1 text-xs text-faint line-through" title="No tag has this name">
                                      {t}
                                    </span>
                                  ))}
                                  {i.tags.length === 0 && <span className="text-xs text-faint">none suggested</span>}
                                </span>
                              </div>
                            )}
                          </td>
                          <td className={cell}>
                            {!i.ask_type ? (
                              <span className="text-xs text-faint">has a type</span>
                            ) : i.has_type_now ? (
                              <span className="text-xs text-faint">typed since</span>
                            ) : (
                              i.type_id ? (
                                <button
                                  type="button"
                                  aria-pressed={!droppedType[i.doc_id]}
                                  title={
                                    droppedType[i.doc_id] ? "Left out. Click to put it back." : "Click to leave this type out."
                                  }
                                  onClick={() => setDroppedType({ ...droppedType, [i.doc_id]: !droppedType[i.doc_id] })}
                                  className={`rounded px-1.5 py-0.5 text-left text-xs ${
                                    droppedType[i.doc_id]
                                      ? "border border-dashed border-line text-faint line-through"
                                      : "bg-surface-2 hover:bg-line"
                                  }`}
                                >
                                  {i.type_name}
                                </button>
                              ) : (
                                <span className="text-xs text-faint line-through">{i.type_name || "none suggested"}</span>
                              )
                            )}
                          </td>
                          <td className="whitespace-nowrap px-2 py-1 text-right">
                            <Button
                              size="sm"
                              variant="ghost"
                              title="Takes it off this list; it is not asked about again until you un-ignore it."
                              onClick={() => ignore([{ doc_id: i.doc_id, title: i.title }])}
                            >
                              Ignore
                            </Button>
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
          {hiddenPicked > 0 && (
            <span className="mr-auto text-xs text-warn">
              {hiddenPicked} of the selected documents {hiddenPicked === 1 ? "is" : "are"} hidden by the filter.
            </span>
          )}
          {picked.length > 0 && (
            <Button onClick={ignorePicked} disabled={busy}>
              Ignore {picked.length} selected
            </Button>
          )}
          <Button onClick={onClose} disabled={busy}>
            Close
          </Button>
          <Button variant="primary" onClick={apply} loading={busy} disabled={!writes || chosen.length === 0}>
            Apply to {chosen.length} selected
          </Button>
        </div>
      </div>
    </Modal>
  );
};

export default FillDialog;
