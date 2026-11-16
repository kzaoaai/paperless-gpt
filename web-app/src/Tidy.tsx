// Fork-only: the Tidy page. Inventory, findings and cleanup actions for
// paperless-ngx tags, correspondents, document types, storage paths and custom
// fields. Every change is logged and can be undone (Go side: tidy_*.go).
import {
  ArrowPathIcon,
  ExclamationTriangleIcon,
  SparklesIcon,
} from "@heroicons/react/24/outline";
import classNames from "classnames";
import React, { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import ActionLog from "./components/tidy/ActionLog";
import MergeDialog from "./components/tidy/MergeDialog";
import SplitDialog from "./components/tidy/SplitDialog";
import FillDialog from "./components/tidy/FillDialog";
import TitlesDialog from "./components/tidy/TitlesDialog";
import {
  Action,
  Finding,
  Inventory,
  KINDS,
  Kind,
  LLMGroup,
  MATCH_NAMES,
  Row,
  TidyError,
  attributesLink,
  docsLink,
  tidyApi,
} from "./components/tidy/api";
import Button from "./components/ui/Button";
import ConfirmDialog from "./components/ui/ConfirmDialog";
import Modal from "./components/ui/Modal";

const FINDING_TITLES: Record<string, string> = {
  near_duplicate: "Possible duplicates",
  unused: "No documents",
  dead_rule: "Matching rule that never matches",
  ignored_rule_text: "Matching text that is ignored",
  hidden_from_viewer: "Hidden from the read-only user",
  stale_reference: "Points at something deleted",
};

type SortKey = "name" | "doc_count" | "last_created" | "last_added";

// Words that appear in nearly every document; "Any word" matching on them
// would match almost everything.
const COMMON_WORDS = new Set(["the", "and", "for", "from", "with", "your", "this", "that", "department", "account", "statement", "invoice", "total"]);

interface LLMState {
  kind: Kind;
  loading: boolean;
  groups: LLMGroup[] | null;
  error?: string;
  picked: Set<number>;
}

const Tidy: React.FC = () => {
  const [params, setParams] = useSearchParams();
  const kind = (KINDS.find((k) => k.kind === params.get("type"))?.kind ?? "correspondents") as Kind;
  const kindInfo = KINDS.find((k) => k.kind === kind)!;

  const [inv, setInv] = useState<Inventory | null>(null);
  const [actions, setActions] = useState<Action[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [paperlessUrl, setPaperlessUrl] = useState("");
  const [filter, setFilter] = useState("");
  const [sort, setSort] = useState<{ key: SortKey; desc: boolean }>({ key: "name", desc: false });

  const [mergeIds, setMergeIds] = useState<number[] | null>(null);
  const [splitRow, setSplitRow] = useState<Row | null>(null);
  const [fillOpen, setFillOpen] = useState(false);
  const [titlesOpen, setTitlesOpen] = useState(false);
  const [renameRow, setRenameRow] = useState<Row | null>(null);
  const [renameTo, setRenameTo] = useState("");
  const [confirm, setConfirm] = useState<{ title: string; body: string; label: string; run: () => Promise<unknown> } | null>(null);
  const [working, setWorking] = useState(false);
  // LLM proposals belong to the tab they were asked for; another tab shows none.
  const [llmState, setLlm] = useState<LLMState>({ kind, loading: false, groups: null, picked: new Set() });
  const llm: LLMState = llmState.kind === kind ? llmState : { kind, loading: false, groups: null, picked: new Set() };

  const fetchAll = (refresh: boolean) =>
    Promise.all([tidyApi.inventory(refresh), tidyApi.actions()])
      .then(([i, a]) => {
        setInv(i);
        setActions(a);
        setError(null);
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));

  const load = (refresh = false) => {
    setLoading(true);
    fetchAll(refresh);
  };

  useEffect(() => {
    Promise.all([tidyApi.inventory(false), tidyApi.actions()])
      .then(([i, a]) => {
        setInv(i);
        setActions(a);
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
    fetch("./api/paperless-url")
      .then((r) => r.json())
      .then((d) => setPaperlessUrl(d.url ?? ""))
      .catch(() => {});
  }, []);

  const changed = (message: string) => {
    setNotice(message);
    load(false);
  };

  const act = async (run: () => Promise<unknown>, message: string) => {
    setWorking(true);
    setError(null);
    try {
      await run();
      changed(message);
    } catch (e) {
      const refs = e instanceof TidyError && e.references?.length ? ` (${e.references.map((r) => r.label).join("; ")})` : "";
      setError((e instanceof Error ? e.message : String(e)) + refs);
    } finally {
      setWorking(false);
    }
  };

  const rows = useMemo(() => inv?.objects[kind] ?? [], [inv, kind]);
  const byId = useMemo(() => new Map(rows.map((r) => [r.id, r])), [rows]);
  const findings = useMemo(() => (inv?.findings ?? []).filter((f) => f.kind === kind), [inv, kind]);
  const writes = inv?.writes ?? false;
  const mergeable = kind !== "custom_fields";
  const hasMatching = kind !== "custom_fields";

  const shown = useMemo(() => {
    const q = filter.trim().toLowerCase();
    const list = rows.filter((r) => !q || r.name.toLowerCase().includes(q) || r.match.toLowerCase().includes(q));
    const dir = sort.desc ? -1 : 1;
    return [...list].sort((a, b) => {
      const va = a[sort.key] ?? "";
      const vb = b[sort.key] ?? "";
      if (typeof va === "number" && typeof vb === "number") return (va - vb) * dir;
      return String(va).localeCompare(String(vb), undefined, { sensitivity: "base" }) * dir;
    });
  }, [rows, filter, sort]);

  const askLLM = async () => {
    setLlm({ kind, loading: true, groups: null, picked: new Set() });
    try {
      const groups = await tidyApi.llmGroups(kind);
      setLlm({ kind, loading: false, groups, picked: new Set() });
    } catch (e) {
      setLlm({ kind, loading: false, groups: null, error: e instanceof Error ? e.message : String(e), picked: new Set() });
    }
  };

  const applyLLM = async () => {
    const groups = (llm.groups ?? []).filter((_, i) => llm.picked.has(i));
    await act(async () => {
      for (const g of groups) {
        for (const id of g.ids) {
          if (id !== g.keep) await tidyApi.merge(kind, id, g.keep);
        }
      }
      setLlm({ kind, loading: false, groups: null, picked: new Set() });
    }, `Merged ${groups.length} group(s)`);
  };

  const missing = {
    tags: inv?.hygiene?.find((h) => h.key === "no_tags")?.count ?? 0,
    types: inv?.hygiene?.find((h) => h.key === "no_document_type")?.count ?? 0,
  };
  const names = (ids: number[]) => ids.map((id) => byId.get(id)?.name ?? `#${id}`);
  const header = (key: SortKey, label: string, className?: string) => (
    <th className={classNames("px-2 py-2 font-medium", className)}>
      <button
        className="hover:text-ink"
        onClick={() => setSort((s) => ({ key, desc: s.key === key ? !s.desc : key !== "name" }))}
      >
        {label}
        {sort.key === key ? (sort.desc ? " ↓" : " ↑") : ""}
      </button>
    </th>
  );

  const findingActions = (f: Finding) => {
    const n = f.ids.length;
    switch (f.type) {
      case "near_duplicate":
        return mergeable ? (
          <Button size="sm" onClick={() => setMergeIds(f.ids)} disabled={!writes}>
            Merge…
          </Button>
        ) : null;
      case "unused": {
        const deletable = f.ids.filter((id) => {
          const r = byId.get(id);
          return r && r.doc_count === 0 && r.references.length === 0 && !rows.some((x) => x.parent === id);
        });
        if (deletable.length < 2) return null;
        return (
          <Button
            size="sm"
            variant="danger"
            disabled={!writes || working}
            onClick={() =>
              setConfirm({
                title: `Delete ${deletable.length} ${kindInfo.label.toLowerCase()}`,
                body: `Delete ${names(deletable).map((x) => `“${x}”`).join(", ")}? None has documents or is used anywhere. This is one logged action, and one Undo brings them all back.${
                  deletable.length < f.ids.length ? ` ${f.ids.length - deletable.length} others are in use and stay.` : ""
                }`,
                label: "Delete all",
                run: () => tidyApi.removeMany(kind, deletable),
              })
            }
          >
            Delete all {deletable.length}
          </Button>
        );
      }
      case "dead_rule":
        return (
          <Button
            size="sm"
            disabled={!writes || working}
            onClick={() =>
              setConfirm({
                title: "Set matching to None",
                body: `Set the matching algorithm of ${n} ${kindInfo.label.toLowerCase()} to None. They never matched anything, so nothing changes in practice; it makes the setting say so.`,
                label: "Set to None",
                run: () => tidyApi.setMatching(kind, f.ids, 0),
              })
            }
          >
            Set to None
          </Button>
        );
      case "ignored_rule_text": {
        const texts = f.ids.map((id) => `${byId.get(id)?.name}: “${byId.get(id)?.match}”`).join("; ");
        const common = f.ids.flatMap((id) =>
          (byId.get(id)?.match ?? "").split(/\s+/).filter((w) => w.length > 0 && (w.length <= 3 || COMMON_WORDS.has(w.toLowerCase())))
        );
        const commonNote = common.length
          ? ` Careful: “${[...new Set(common)].join("”, “")}” appears in almost every document, so any-word matching would assign them nearly everywhere.`
          : "";
        const byAlgo = new Map<number, number[]>();
        f.ids.forEach((id) => {
          const algo = byId.get(id)?.matching_algorithm ?? 6;
          byAlgo.set(algo, [...(byAlgo.get(algo) ?? []), id]);
        });
        return (
          <div className="flex flex-wrap gap-2">
            <Button
              size="sm"
              disabled={!writes || working}
              onClick={() =>
                setConfirm({
                  title: "Match the exact text",
                  body: `Switch ${n} to "Exact match": paperless-ngx then assigns each to new documents containing that exact text, instead of its learned (Automatic) matching. ${texts}.`,
                  label: "Use Exact match",
                  run: () => tidyApi.setMatching(kind, f.ids, 3),
                })
              }
            >
              Exact text
            </Button>
            <Button
              size="sm"
              disabled={!writes || working}
              onClick={() =>
                setConfirm({
                  title: "Match any of the words",
                  body: `Switch ${n} to "Any word": paperless-ngx then assigns each to new documents containing any one of its words. ${texts}.${commonNote}`,
                  label: "Use Any word",
                  run: () => tidyApi.setMatching(kind, f.ids, 1),
                })
              }
            >
              Any word
            </Button>
            <Button
              size="sm"
              variant="ghost"
              disabled={!writes || working}
              onClick={() =>
                setConfirm({
                  title: "Clear the matching text",
                  body: `Remove the unused matching text from ${n}, keeping their algorithm, so nothing changes in how they match. ${texts}.`,
                  label: "Clear text",
                  run: async () => {
                    for (const [algo, ids] of byAlgo) await tidyApi.setMatching(kind, ids, algo, "");
                  },
                })
              }
            >
              Clear the text
            </Button>
          </div>
        );
      }
      case "hidden_from_viewer":
        return (
          <Button
            size="sm"
            disabled={!writes || working}
            onClick={() =>
              setConfirm({
                title: `Let ${inv?.viewer} view them`,
                body: `Add view permission for ${inv?.viewer} to ${n} ${kindInfo.label.toLowerCase()}. Owner and other permissions stay as they are.`,
                label: "Grant view",
                run: () => tidyApi.grantViewer(kind, f.ids),
              })
            }
          >
            Grant view
          </Button>
        );
      default:
        return null;
    }
  };

  return (
    <div className="mx-auto max-w-6xl space-y-6 px-4 py-8 sm:px-6">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="max-w-2xl">
          <h1 className="text-xl font-semibold">Tidy</h1>
          <p className="mt-1 text-sm text-muted">
            Find and clean up duplicate, unused and misconfigured tags, correspondents, document types, storage paths and
            custom fields in paperless-ngx. Every change is logged below and can be undone.
          </p>
        </div>
        <div className="flex items-center gap-2">
          {inv && !writes && (
            <span className="rounded-md bg-warn-tint px-2 py-1 text-xs text-warn">Read-only (TIDY_WRITES is off)</span>
          )}
          <Button size="sm" onClick={() => load(true)} loading={loading}>
            <ArrowPathIcon className="h-4 w-4" aria-hidden="true" /> Refresh
          </Button>
        </div>
      </div>

      {(inv?.warnings ?? []).map((w) => (
        <p key={w} className="flex gap-2 text-sm text-warn">
          <ExclamationTriangleIcon className="mt-0.5 h-4 w-4 shrink-0" aria-hidden="true" /> {w}
        </p>
      ))}
      {error && <p className="rounded-md bg-neg-tint px-3 py-2 text-sm text-neg">{error}</p>}
      {notice && (
        <p className="flex justify-between rounded-md bg-pos-tint px-3 py-2 text-sm text-pos">
          {notice}
          <button className="text-xs" onClick={() => setNotice(null)}>
            dismiss
          </button>
        </p>
      )}

      {inv && (inv.hygiene ?? []).length > 0 && (
        <section className="space-y-2">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h2 className="text-sm font-semibold">
              Archive health <span className="font-normal text-muted">({inv.document_count} documents)</span>
            </h2>
            <div className="flex flex-wrap gap-2">
              <Button size="sm" variant="ghost" onClick={() => setTitlesOpen(true)}>
                <SparklesIcon className="h-4 w-4" aria-hidden="true" /> Re-check titles…
              </Button>
              {missing.tags + missing.types > 0 && (
                <Button size="sm" variant="ghost" onClick={() => setFillOpen(true)}>
                  <SparklesIcon className="h-4 w-4" aria-hidden="true" /> Fill missing tags and types…
                </Button>
              )}
            </div>
          </div>
          <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
            {(inv.hygiene ?? []).map((h) => {
              const href = paperlessUrl && h.query ? `${paperlessUrl}/documents?${h.query}` : undefined;
              const body = (
                <>
                  <span className={classNames("block text-lg font-semibold tabular-nums", h.count > 0 ? "text-ink" : "text-faint")}>
                    {h.count}
                  </span>
                  <span className="block text-xs text-muted">{h.label}</span>
                </>
              );
              return href && h.count > 0 ? (
                <a key={h.key} href={href} target="_blank" rel="noreferrer" className="rounded-md border border-line px-3 py-2 hover:bg-surface-2">
                  {body}
                </a>
              ) : (
                <div key={h.key} className="rounded-md border border-line px-3 py-2">
                  {body}
                </div>
              );
            })}
          </div>
        </section>
      )}

      <nav className="flex flex-wrap gap-1 border-b border-line" aria-label="Object type">
        {KINDS.map((k) => (
          <button
            key={k.kind}
            onClick={() => setParams({ type: k.kind })}
            className={classNames(
              "-mb-px border-b-2 px-3 py-2 text-sm",
              k.kind === kind ? "border-primary font-medium text-ink" : "border-transparent text-muted hover:text-ink"
            )}
          >
            {k.label}
            {inv && <span className="ml-1 text-xs text-faint">{inv.objects[k.kind]?.length ?? 0}</span>}
          </button>
        ))}
      </nav>

      {loading && !inv && <p className="text-sm text-muted">Reading paperless-ngx…</p>}

      {inv && (
        <>
          <section className="space-y-3">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <h2 className="text-sm font-semibold">Findings</h2>
              {mergeable && (
                <Button size="sm" variant="ghost" onClick={askLLM} loading={llm.loading}>
                  <SparklesIcon className="h-4 w-4" aria-hidden="true" /> Ask the LLM for duplicates
                </Button>
              )}
            </div>
            {llm.error && <p className="text-sm text-neg">{llm.error}</p>}
            {llm.groups && (
              <div className="space-y-2 rounded-md border border-primary px-3 py-2 text-sm">
                <p className="text-muted">
                  The LLM's proposals. Nothing is merged until you tick a group and apply it; the bold name is kept.
                </p>
                {llm.groups.length === 0 && <p>No duplicates proposed.</p>}
                {llm.groups.map((g, i) => (
                  <label key={i} className="flex items-start gap-2">
                    <input
                      type="checkbox"
                      className="mt-1 accent-[var(--color-primary)]"
                      checked={llm.picked.has(i)}
                      onChange={(e) => {
                        const picked = new Set(llm.picked);
                        if (e.target.checked) picked.add(i);
                        else picked.delete(i);
                        setLlm({ ...llm, picked });
                      }}
                    />
                    <span>
                      {g.ids.map((id, j) => (
                        <React.Fragment key={id}>
                          {j > 0 && " · "}
                          <span className={id === g.keep ? "font-semibold" : ""}>{byId.get(id)?.name ?? id}</span>
                        </React.Fragment>
                      ))}
                      {g.reason && <span className="block text-xs text-muted">{g.reason}</span>}
                    </span>
                  </label>
                ))}
                {llm.groups.length > 0 && (
                  <Button size="sm" variant="primary" disabled={!writes || llm.picked.size === 0 || working} onClick={applyLLM}>
                    Merge {llm.picked.size} ticked
                  </Button>
                )}
              </div>
            )}
            {findings.length === 0 && <p className="text-sm text-muted">Nothing found.</p>}
            <ul className="space-y-2">
              {findings.map((f, i) => (
                <li key={i} className="rounded-md border border-line px-3 py-2 text-sm">
                  <div className="flex flex-wrap items-start justify-between gap-2">
                    <div className="min-w-0 flex-1">
                      <p className="font-medium">{FINDING_TITLES[f.type] ?? f.type}</p>
                      <p className="text-xs text-muted">{f.detail}</p>
                      {f.ids.length > 0 && (
                        <p className="mt-1 flex flex-wrap gap-1">
                          {f.ids.map((id) => {
                            const r = byId.get(id);
                            return (
                              <span key={id} className="rounded bg-surface-2 px-1.5 py-0.5 text-xs">
                                {r?.name ?? `#${id}`}
                                {r && <span className="ml-1 text-faint">{r.doc_count}</span>}
                                {f.type === "unused" && r && r.references.length === 0 && writes && (
                                  <button
                                    className="ml-1 text-neg hover:underline"
                                    onClick={() =>
                                      setConfirm({
                                        title: `Delete ${kindInfo.singular}`,
                                        body: `Delete “${r.name}”? It has no documents. The log keeps everything about it, so this can be undone.`,
                                        label: "Delete",
                                        run: () => tidyApi.remove(kind, r.id),
                                      })
                                    }
                                  >
                                    delete
                                  </button>
                                )}
                              </span>
                            );
                          })}
                        </p>
                      )}
                    </div>
                    {findingActions(f)}
                  </div>
                </li>
              ))}
            </ul>
          </section>

          {kind === "custom_fields" && (inv.field_audit ?? []).length > 0 && (
            <section className="space-y-3">
              <h2 className="text-sm font-semibold">Values</h2>
              <p className="text-xs text-muted">
                What each field holds, its most common values, values that differ only in case, spacing or punctuation,
                and select options nothing uses.
              </p>
              <div className="grid gap-3 lg:grid-cols-2">
                {(inv.field_audit ?? []).map((a) => (
                  <div key={a.id} className="space-y-2 rounded-md border border-line px-3 py-2 text-sm">
                    <p className="font-medium">
                      {a.name} <span className="text-xs font-normal text-faint">{a.data_type}</span>
                    </p>
                    <p className="text-xs text-muted">
                      On {a.doc_count} documents, {a.empty_count} of them empty; {a.distinct} different values
                      {a.no_currency ? `; ${a.no_currency} without a currency code` : ""}.
                    </p>
                    {a.variants.length > 0 && (
                      <div className="text-xs">
                        <p className="text-warn">Written differently:</p>
                        <ul className="ml-4 list-disc">
                          {a.variants.map((g, i) => (
                            <li key={i}>{g.map((v) => `“${v.value}” (${v.count})`).join(" · ")}</li>
                          ))}
                        </ul>
                      </div>
                    )}
                    {a.unused_options.length > 0 && (
                      <p className="text-xs text-warn">Unused options: {a.unused_options.join(", ")}</p>
                    )}
                    {a.top.length > 0 && a.data_type !== "documentlink" && (
                      <details className="text-xs">
                        <summary className="cursor-pointer text-muted">
                          Most common values{a.distinct > a.top.length ? ` (top ${a.top.length})` : ""}
                        </summary>
                        <ul className="mt-1 space-y-0.5">
                          {a.top.map((v) => (
                            <li key={v.value} className="flex justify-between gap-2">
                              <span className="truncate">{v.value}</span>
                              <span className="tabular-nums text-faint">{v.count}</span>
                            </li>
                          ))}
                        </ul>
                      </details>
                    )}
                  </div>
                ))}
              </div>
            </section>
          )}

          <section className="space-y-3">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <h2 className="text-sm font-semibold">
                {kindInfo.label} <span className="font-normal text-muted">({rows.length}, {inv.document_count} documents scanned)</span>
              </h2>
              <input
                className="w-56 rounded-md border border-line bg-surface px-2 py-1 text-sm"
                placeholder="Filter by name or match text"
                value={filter}
                onChange={(e) => setFilter(e.target.value)}
              />
            </div>
            {hasMatching && inv.matching_summary[kind] && (
              <p className="text-xs text-muted">
                Matching:{" "}
                {Object.entries(inv.matching_summary[kind])
                  .sort((a, b) => b[1] - a[1])
                  .map(([name, n]) => `${name} ${n}`)
                  .join(" · ")}
              </p>
            )}
            <div className="overflow-x-auto rounded-md border border-line">
              <table className="w-full text-left text-sm">
                <thead className="border-b border-line bg-surface-2 text-xs text-muted">
                  <tr>
                    {header("name", "Name")}
                    {header("doc_count", "Docs", "text-right")}
                    {header("last_created", "Newest created")}
                    {header("last_added", "Last added")}
                    {hasMatching && <th className="px-2 py-2 font-medium">Matching</th>}
                    {kind === "custom_fields" && <th className="px-2 py-2 font-medium">Type</th>}
                    {kind !== "custom_fields" && <th className="px-2 py-2 font-medium">Owner</th>}
                    {kind === "tags" && <th className="px-2 py-2 font-medium">Parent</th>}
                    <th className="px-2 py-2 font-medium">Used by</th>
                    <th className="px-2 py-2" />
                  </tr>
                </thead>
                <tbody className="divide-y divide-line">
                  {shown.map((r) => (
                    <tr key={r.id} className="align-top">
                      <td className="px-2 py-1.5">
                        {paperlessUrl && r.doc_count > 0 ? (
                          <a
                            className="text-primary hover:underline"
                            href={docsLink(paperlessUrl, kind, r.id)}
                            target="_blank"
                            rel="noreferrer"
                            title={`Open its ${r.doc_count} document${r.doc_count === 1 ? "" : "s"} in paperless-ngx`}
                          >
                            {r.name}
                          </a>
                        ) : (
                          r.name
                        )}
                        <span className="ml-1 text-xs text-faint">#{r.id}</span>
                        {r.is_inbox_tag && <span className="ml-1 text-xs text-primary">inbox</span>}
                      </td>
                      <td className="px-2 py-1.5 text-right tabular-nums">
                        {paperlessUrl && r.doc_count > 0 ? (
                          <a className="hover:underline" href={docsLink(paperlessUrl, kind, r.id)} target="_blank" rel="noreferrer">
                            {r.doc_count}
                          </a>
                        ) : (
                          r.doc_count
                        )}
                      </td>
                      <td className="px-2 py-1.5 text-muted">{r.last_created ?? "—"}</td>
                      <td className="px-2 py-1.5 text-muted">{r.last_added ? r.last_added.slice(0, 10) : "—"}</td>
                      {hasMatching && (
                        <td className="px-2 py-1.5">
                          {paperlessUrl ? (
                            <a
                              className="hover:underline"
                              href={attributesLink(paperlessUrl, kind)}
                              target="_blank"
                              rel="noreferrer"
                              title={`Open paperless-ngx's ${kindInfo.label} page to edit “${r.name}”`}
                            >
                              {MATCH_NAMES[r.matching_algorithm ?? -1] ?? "—"}
                              {r.match && <span className="block text-xs text-muted">“{r.match}”</span>}
                            </a>
                          ) : (
                            <>
                              {MATCH_NAMES[r.matching_algorithm ?? -1] ?? "—"}
                              {r.match && <span className="block text-xs text-muted">“{r.match}”</span>}
                            </>
                          )}
                        </td>
                      )}
                      {kind === "custom_fields" && <td className="px-2 py-1.5 text-muted">{r.data_type}</td>}
                      {kind !== "custom_fields" && (
                        <td className="px-2 py-1.5">
                          {r.owner === null ? <span className="text-muted">none</span> : r.owner_name || `#${r.owner}`}
                          {r.visible_to_viewer === false && (
                            <span className="block text-xs text-warn">hidden from {inv.viewer}</span>
                          )}
                        </td>
                      )}
                      {kind === "tags" && <td className="px-2 py-1.5 text-muted">{r.parent ? names([r.parent])[0] : ""}</td>}
                      <td className="px-2 py-1.5 text-xs text-muted">
                        {r.references.length > 0 ? (
                          <span title={r.references.map((x) => x.label).join("\n")}>
                            {r.references.length === 1 ? r.references[0].label : `${r.references.length} places`}
                          </span>
                        ) : (
                          ""
                        )}
                      </td>
                      <td className="whitespace-nowrap px-2 py-1.5 text-right">
                        {kind === "correspondents" && r.doc_count >= 2 && (
                          <Button size="sm" variant="ghost" onClick={() => setSplitRow(r)}>
                            Split…
                          </Button>
                        )}
                        {writes && (
                          <>
                            <Button
                              size="sm"
                              variant="ghost"
                              onClick={() => {
                                setRenameRow(r);
                                setRenameTo(r.name);
                              }}
                            >
                              Rename
                            </Button>
                            {mergeable && (
                              <Button size="sm" variant="ghost" onClick={() => setMergeIds([r.id])}>
                                Merge…
                              </Button>
                            )}
                            {r.doc_count === 0 && r.references.length === 0 && (
                              <Button
                                size="sm"
                                variant="ghost"
                                onClick={() =>
                                  setConfirm({
                                    title: `Delete ${kindInfo.singular}`,
                                    body: `Delete “${r.name}”? It has no documents. The log keeps everything about it, so this can be undone.`,
                                    label: "Delete",
                                    run: () => tidyApi.remove(kind, r.id),
                                  })
                                }
                              >
                                Delete
                              </Button>
                            )}
                          </>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </section>

          <section className="space-y-3">
            <h2 className="text-sm font-semibold">Log</h2>
            <ActionLog actions={actions} writes={writes} onChanged={changed} />
          </section>

          <section className="space-y-3">
            <h2 className="text-sm font-semibold">Merged-away names</h2>
            <p className="text-xs text-muted">
              When the LLM suggests one of these correspondent or document type names, paperless-gpt uses the object it was
              merged into instead of creating the name again.
            </p>
            {inv.aliases.length === 0 ? (
              <p className="text-sm text-muted">None yet.</p>
            ) : (
              <ul className="divide-y divide-line rounded-md border border-line text-sm">
                {inv.aliases.map((a) => {
                  const target = inv.objects[a.kind]?.find((r) => r.id === a.target_id);
                  return (
                    <li key={a.id} className="flex items-center justify-between gap-2 px-3 py-1.5">
                      <span>
                        “{a.name}” → {target?.name ?? <span className="text-warn">#{a.target_id} (gone)</span>}
                        <span className="ml-2 text-xs text-faint">{KINDS.find((k) => k.kind === a.kind)?.singular}</span>
                      </span>
                      {writes && (
                        <Button
                          size="sm"
                          variant="ghost"
                          onClick={() => act(() => tidyApi.deleteAlias(a.id), `Removed the alias “${a.name}”`)}
                        >
                          Remove
                        </Button>
                      )}
                    </li>
                  );
                })}
              </ul>
            )}
          </section>
        </>
      )}

      {inv && titlesOpen && (
        <TitlesDialog
          writes={writes}
          paperlessUrl={paperlessUrl}
          onClose={() => setTitlesOpen(false)}
          onDone={(m) => {
            setTitlesOpen(false);
            changed(m);
          }}
        />
      )}
      {inv && fillOpen && (
        <FillDialog
          missingTags={missing.tags}
          missingTypes={missing.types}
          writes={writes}
          paperlessUrl={paperlessUrl}
          onClose={() => setFillOpen(false)}
          onDone={(m) => {
            setFillOpen(false);
            changed(m);
          }}
        />
      )}

      {inv && splitRow && (
        <SplitDialog
          bucket={splitRow}
          rows={inv.objects.correspondents}
          writes={writes}
          paperlessUrl={paperlessUrl}
          onClose={() => setSplitRow(null)}
          onDone={(m) => {
            setSplitRow(null);
            changed(m);
          }}
        />
      )}

      {inv && mergeIds && (
        <MergeDialog
          open
          kind={kind}
          rows={rows}
          ids={mergeIds}
          writes={writes}
          onClose={() => setMergeIds(null)}
          onDone={(m) => {
            setMergeIds(null);
            changed(m);
          }}
        />
      )}

      <Modal open={renameRow !== null} onClose={() => setRenameRow(null)} title={`Rename ${kindInfo.singular}`}>
        <div className="space-y-2 px-6 py-4">
          <input
            className="w-full rounded-md border border-line bg-surface px-2 py-1 text-sm"
            value={renameTo}
            onChange={(e) => setRenameTo(e.target.value)}
            autoFocus
          />
        </div>
        <div className="flex justify-end gap-2 border-t border-line px-6 py-4">
          <Button onClick={() => setRenameRow(null)}>Cancel</Button>
          <Button
            variant="primary"
            loading={working}
            disabled={!renameTo.trim() || renameTo.trim() === renameRow?.name}
            onClick={() => {
              const r = renameRow!;
              setRenameRow(null);
              act(() => tidyApi.rename(kind, r.id, renameTo.trim()), `Renamed “${r.name}” to “${renameTo.trim()}”`);
            }}
          >
            Rename
          </Button>
        </div>
      </Modal>

      <ConfirmDialog
        open={confirm !== null}
        title={confirm?.title ?? ""}
        body={confirm?.body ?? ""}
        confirmLabel={confirm?.label ?? ""}
        onCancel={() => setConfirm(null)}
        onConfirm={() => {
          const c = confirm!;
          setConfirm(null);
          act(c.run, `${c.label}: done`);
        }}
      />
    </div>
  );
};

export default Tidy;
