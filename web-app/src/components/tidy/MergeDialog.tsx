import { ExclamationTriangleIcon } from "@heroicons/react/24/outline";
import React, { useEffect, useState } from "react";
import Button from "../ui/Button";
import Modal from "../ui/Modal";
import { Kind, MergePlan, Row, TidyError, tidyApi } from "./api";

interface MergeDialogProps {
  open: boolean;
  kind: Kind;
  rows: Row[];
  /** Objects to merge; the first one is the default target. */
  ids: number[];
  writes: boolean;
  onClose: () => void;
  onDone: (message: string) => void;
}

/**
 * Merges each chosen object into one target. Every source is previewed first:
 * its documents, child tags, what still uses it, and the Document Updated
 * workflows the move will set off.
 */
const MergeDialog: React.FC<MergeDialogProps> = ({ open, kind, rows, ids, writes, onClose, onDone }) => {
  const byId = new Map(rows.map((r) => [r.id, r]));
  // Default target: the one with the most documents. The dialog is mounted
  // afresh for each merge, so this runs once per merge.
  const [target, setTarget] = useState<number>(
    () => [...ids].sort((a, b) => (byId.get(b)?.doc_count ?? 0) - (byId.get(a)?.doc_count ?? 0))[0] ?? 0
  );
  const [extra, setExtra] = useState<number>(0);
  // Previews keyed by "target:source", so changing the target needs no reset.
  const [plans, setPlans] = useState<Record<string, MergePlan | string>>({});
  const [running, setRunning] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const members = extra ? [...ids, extra] : ids;
  const sources = members.filter((id) => id !== target);
  const planOf = (src: number) => plans[`${target}:${src}`];

  useEffect(() => {
    if (!open || !target) return;
    let cancelled = false;
    sources.forEach((src) => {
      const key = `${target}:${src}`;
      tidyApi
        .previewMerge(kind, src, target)
        .then((p) => !cancelled && setPlans((prev) => ({ ...prev, [key]: p })))
        .catch((e: Error) => !cancelled && setPlans((prev) => ({ ...prev, [key]: e.message })));
    });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, target, extra, kind]);

  const ready =
    sources.length > 0 && sources.every((s) => typeof planOf(s) === "object" && !(planOf(s) as MergePlan).blocked);

  const run = async () => {
    setRunning(true);
    setError(null);
    const done: string[] = [];
    try {
      for (const src of sources) {
        await tidyApi.merge(kind, src, target);
        done.push(byId.get(src)?.name ?? String(src));
      }
      onDone(`Merged ${done.join(", ")} into ${byId.get(target)?.name}`);
    } catch (e) {
      const msg = e instanceof TidyError ? e.message : String(e);
      setError(done.length ? `Merged ${done.join(", ")}; then: ${msg}` : msg);
    } finally {
      setRunning(false);
    }
  };

  const workflows = new Map<number, string>();
  sources.map(planOf).forEach((p) => {
    if (typeof p === "object") (p.updated_workflows ?? []).forEach((w) => w.enabled && workflows.set(w.id, w.name));
  });

  return (
    <Modal open={open} onClose={running ? () => {} : onClose} title="Merge" size="lg">
      <div className="space-y-4 px-6 py-4 text-sm">
        <div>
          <p className="mb-2 text-muted">Keep this one; the others are merged into it and deleted once empty:</p>
          <div className="space-y-1">
            {members.map((id) => (
              <label key={id} className="flex items-center gap-2">
                <input
                  type="radio"
                  className="accent-[var(--color-primary)]"
                  checked={target === id}
                  onChange={() => setTarget(id)}
                  disabled={running}
                />
                <span className="font-medium">{byId.get(id)?.name ?? id}</span>
                <span className="text-xs text-faint">#{id} · {byId.get(id)?.doc_count ?? 0} documents</span>
              </label>
            ))}
          </div>
          {ids.length === 1 && (
            <label className="mt-3 flex items-center gap-2">
              <span className="text-muted">Merge with</span>
              <select
                className="rounded-md border border-line bg-surface px-2 py-1"
                value={extra}
                onChange={(e) => setExtra(Number(e.target.value))}
                disabled={running}
              >
                <option value={0}>choose…</option>
                {rows
                  .filter((r) => r.id !== ids[0])
                  .map((r) => (
                    <option key={r.id} value={r.id}>
                      {r.name} ({r.doc_count})
                    </option>
                  ))}
              </select>
            </label>
          )}
        </div>

        {sources.map((src) => {
          const p = planOf(src);
          return (
            <div key={src} className="rounded-md border border-line px-3 py-2">
              <p className="font-medium">
                {byId.get(src)?.name} → {byId.get(target)?.name}
              </p>
              {p === undefined && <p className="text-muted">Checking…</p>}
              {typeof p === "string" && <p className="text-neg">{p}</p>}
              {typeof p === "object" && (
                <ul className="mt-1 space-y-0.5 text-muted">
                  <li>{(p.docs ?? []).length} documents move</li>
                  {(p.children ?? []).length > 0 && (
                    <li>{p.children!.length} child tags move under {byId.get(target)?.name}</li>
                  )}
                  {p.alias && <li>paperless-gpt will map the name “{p.alias}” to {byId.get(target)?.name}</li>}
                  {p.blocked && (
                    <li className="text-neg">
                      {p.blocked}
                      <ul className="ml-4 list-disc">
                        {p.references.map((r, i) => (
                          <li key={i}>{r.label}</li>
                        ))}
                      </ul>
                    </li>
                  )}
                </ul>
              )}
            </div>
          );
        })}

        {workflows.size > 0 && (
          <p className="flex gap-2 text-warn">
            <ExclamationTriangleIcon className="mt-0.5 h-4 w-4 shrink-0" aria-hidden="true" />
            <span>
              Moving documents counts as an update in paperless-ngx, so these Document Updated workflows run on
              them: {[...workflows.values()].join(", ")}.
            </span>
          </p>
        )}
        {!writes && <p className="text-warn">Read-only: set TIDY_WRITES=true to merge.</p>}
        {error && <p className="text-neg">{error}</p>}
      </div>
      <div className="flex justify-end gap-2 border-t border-line px-6 py-4">
        <Button variant="secondary" onClick={onClose} disabled={running}>
          Cancel
        </Button>
        <Button variant="primary" onClick={run} loading={running} disabled={!ready || !writes}>
          Merge {sources.length > 1 ? `${sources.length} into one` : ""}
        </Button>
      </div>
    </Modal>
  );
};

export default MergeDialog;
