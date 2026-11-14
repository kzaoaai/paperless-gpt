import classNames from "classnames";
import React, { useState } from "react";
import Button from "../ui/Button";
import ConfirmDialog from "../ui/ConfirmDialog";
import { Action, KINDS, TidyError, tidyApi } from "./api";

interface ActionLogProps {
  actions: Action[];
  writes: boolean;
  onChanged: (message: string) => void;
}

const statusClass: Record<string, string> = {
  done: "text-pos",
  partial: "text-warn",
  failed: "text-neg",
  started: "text-warn",
};

/** Every Tidy action, newest first, each with an undo. */
const ActionLog: React.FC<ActionLogProps> = ({ actions, writes, onChanged }) => {
  const [busy, setBusy] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [drift, setDrift] = useState<{ id: number; message: string } | null>(null);

  const undo = async (id: number, force = false) => {
    setBusy(id);
    setError(null);
    try {
      const a = await tidyApi.undo(id, force);
      onChanged(a.undo_note || "Undone");
    } catch (e) {
      if (e instanceof TidyError && e.driftedDocs?.length) {
        setDrift({ id, message: `${e.message} Changed documents: ${e.driftedDocs.join(", ")}.` });
      } else {
        setError(e instanceof Error ? e.message : String(e));
      }
    } finally {
      setBusy(null);
    }
  };

  if (actions.length === 0) {
    return <p className="text-sm text-muted">Nothing done yet.</p>;
  }
  return (
    <div className="space-y-2">
      {error && <p className="text-sm text-neg">{error}</p>}
      <ul className="divide-y divide-line rounded-md border border-line">
        {actions.map((a) => (
          <li key={a.id} className="flex flex-wrap items-start justify-between gap-3 px-3 py-2 text-sm">
            <div className="min-w-0 flex-1">
              <p className={classNames(a.undone && "text-faint line-through")}>
                {a.summary}
                <span className="ml-2 text-xs text-faint">
                  {KINDS.find((k) => k.kind === a.kind)?.label} · {new Date(a.created_at).toLocaleString()}
                </span>
              </p>
              <p className="text-xs">
                <span className={statusClass[a.status] ?? "text-muted"}>{a.status}</span>
                {a.error && <span className="ml-2 text-neg">{a.error}</span>}
              </p>
              {a.undone && (
                <p className="text-xs text-muted">
                  Undone {a.undone_at && new Date(a.undone_at).toLocaleString()}: {a.undo_note}
                </p>
              )}
            </div>
            {!a.undone && a.status !== "failed" && (
              <Button size="sm" variant="ghost" loading={busy === a.id} disabled={!writes} onClick={() => undo(a.id)}>
                Undo
              </Button>
            )}
          </li>
        ))}
      </ul>
      <ConfirmDialog
        open={drift !== null}
        title="Some documents changed since"
        body={drift?.message ?? ""}
        confirmLabel="Undo the rest"
        onCancel={() => setDrift(null)}
        onConfirm={() => {
          const id = drift!.id;
          setDrift(null);
          undo(id, true);
        }}
      />
    </div>
  );
};

export default ActionLog;
