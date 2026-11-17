import React, { useEffect, useRef, useState } from "react";
import Button from "../ui/Button";
import Modal from "../ui/Modal";
import { FailedDoc, FailedField, FailedView, tidyApi } from "./api";

interface FailedDialogProps {
  paperlessUrl: string;
  /** dirty: a document was changed, so the page's counts and log are stale */
  onClose: (dirty: boolean) => void;
}

const fieldKey = (d: FailedDoc, f: FailedField) => `${d.doc_id}:${f.field}:${f.field_id ?? 0}`;

/** A timestamp as the day and minute, in the browser's time zone. */
const when = (s: string | undefined) => {
  if (!s) return "";
  const t = new Date(s);
  return Number.isNaN(t.getTime()) ? s : t.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
};

/**
 * Documents paperless-gpt marked failed. The tag alone says nothing, so each
 * document comes with what paperless-gpt recorded: the value paperless-ngx
 * refused and its message, or why processing stopped. From here a refused
 * value can be corrected, the mark cleared, or the document queued again.
 */
const FailedDialog: React.FC<FailedDialogProps> = ({ paperlessUrl, onClose }) => {
  const [view, setView] = useState<FailedView | null>(null);
  const [values, setValues] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState<string | null>(null);
  const dirty = useRef(false);

  const fail = (e: unknown) => setError(e instanceof Error ? e.message : String(e));
  const show = (v: FailedView) => {
    setView(v);
    // Start each editable field from what was refused, keeping anything already typed.
    setValues((old) => {
      const next: Record<string, string> = {};
      for (const d of v.items) {
        for (const f of d.fields) {
          if (f.editable) next[fieldKey(d, f)] = old[fieldKey(d, f)] ?? f.sent;
        }
      }
      return next;
    });
  };

  useEffect(() => {
    tidyApi
      .failed()
      .then(show)
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, []);

  const change = async (run: () => Promise<{ summary: string }>) => {
    setBusy(true);
    setError(null);
    setDone(null);
    try {
      const action = await run();
      dirty.current = true;
      setDone(`${action.summary}. It is in the log below the page, where it can be undone.`);
      show(await tidyApi.failed());
    } catch (e) {
      fail(e);
    } finally {
      setBusy(false);
    }
  };

  const writes = view?.writes ?? false;
  const link = (id: number) => (paperlessUrl ? `${paperlessUrl}/documents/${id}/details` : undefined);
  const tooLong = (d: FailedDoc, f: FailedField) => !!f.max_length && [...(values[fieldKey(d, f)] ?? "")].length > f.max_length;
  const editable = (d: FailedDoc) => d.fields.filter((f) => f.editable);
  const canSave = (d: FailedDoc) =>
    editable(d).length > 0 && editable(d).every((f) => (values[fieldKey(d, f)] ?? "").trim() !== "" && !tooLong(d, f));
  const save = (d: FailedDoc) =>
    change(() =>
      tidyApi.failedFix(
        d.doc_id,
        editable(d).map((f) => ({ field: f.field, field_id: f.field_id, value: values[fieldKey(d, f)] ?? "" })),
      ),
    );

  return (
    <Modal open onClose={busy ? () => {} : () => onClose(dirty.current)} title="Marked failed by paperless-gpt" size="focus">
      <div className="flex min-h-0 flex-1 flex-col">
        <div className="flex-1 space-y-4 overflow-y-auto px-6 py-4 text-sm">
          <p className="text-muted">
            paperless-gpt puts the tag {view?.fail_tag ? `"${view.fail_tag}"` : "it is configured with"} on a document
            when paperless-ngx refused one of the values it suggested, or when it gave up on the document. Each one
            below says which, and what to do about it. Run again sends a document back through paperless-gpt, which
            asks the model again and overwrites what it set before. Clear the mark only removes the tag.
          </p>
          {!view && !error && <p className="text-muted">Loading…</p>}
          {view && !writes && <p className="text-warn">Tidy is read-only (TIDY_WRITES is off), so nothing can be changed from here.</p>}
          {done && <p className="text-pos">{done}</p>}
          {error && <p className="text-neg">{error}</p>}
          {view && view.items.length === 0 && <p className="text-faint">No document is marked failed.</p>}

          {(view?.items ?? []).map((d) => (
            <section key={d.doc_id} className="space-y-3 rounded-md border border-line p-4">
              <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
                <h3 className="font-semibold">
                  {link(d.doc_id) ? (
                    <a className="text-primary hover:underline" href={link(d.doc_id)} target="_blank" rel="noreferrer">
                      #{d.doc_id}
                    </a>
                  ) : (
                    `#${d.doc_id}`
                  )}{" "}
                  {d.title}
                </h3>
                <span className="text-xs text-faint">
                  {d.failed_at ? `marked ${when(d.failed_at)}` : `added ${when(d.added)}`}
                </span>
              </div>
              <p>{d.summary}</p>
              {d.advice && <p className="text-muted">{d.advice}</p>}

              {d.fields.map((f) => {
                const key = fieldKey(d, f);
                const value = values[key] ?? "";
                const length = [...value].length;
                return (
                  <div key={key} className="space-y-1 rounded-md bg-surface-2 p-3">
                    <p className="font-medium">{f.name}</p>
                    {f.error && <p className="text-muted">paperless-ngx answered: {f.error}</p>}
                    <p className="text-muted">{f.advice}</p>
                    {f.current && <p className="text-muted">The document now holds: {f.current}</p>}
                    {f.editable ? (
                      <>
                        <textarea
                          aria-label={`${f.name} for document ${d.doc_id}`}
                          className="w-full rounded-md border border-line bg-surface p-2"
                          rows={f.max_length || value.length > 80 ? 3 : 1}
                          value={value}
                          disabled={busy || !writes}
                          onChange={(e) => setValues((old) => ({ ...old, [key]: e.target.value }))}
                        />
                        {!!f.max_length && (
                          <p className={tooLong(d, f) ? "text-xs text-neg" : "text-xs text-faint"}>
                            {length} of {f.max_length} characters
                            {tooLong(d, f) && `: ${length - f.max_length} too many`}
                          </p>
                        )}
                        {!f.sent && <p className="text-xs text-faint">What paperless-gpt sent was not recorded.</p>}
                      </>
                    ) : (
                      f.sent && <p className="text-muted">paperless-gpt sent: {f.sent}</p>
                    )}
                  </div>
                );
              })}

              <div className="flex flex-wrap items-center gap-2">
                {editable(d).length > 0 && (
                  <Button size="sm" variant="primary" onClick={() => save(d)} disabled={busy || !writes || !canSave(d)}>
                    Save and clear the mark
                  </Button>
                )}
                <Button
                  size="sm"
                  onClick={() => change(() => tidyApi.failedRetry([d.doc_id]))}
                  disabled={busy || !writes || !d.retry_tag}
                  title={d.retry_tag ? `Removes the mark and adds the tag "${d.retry_tag}"` : "The tag that queues a document no longer exists"}
                >
                  Run again
                </Button>
                <Button size="sm" variant="ghost" onClick={() => change(() => tidyApi.failedClear([d.doc_id]))} disabled={busy || !writes}>
                  Clear the mark
                </Button>
              </div>
            </section>
          ))}
        </div>
        <div className="flex items-center justify-end gap-2 border-t border-line px-6 py-4">
          <Button onClick={() => onClose(dirty.current)} disabled={busy}>
            Close
          </Button>
        </div>
      </div>
    </Modal>
  );
};

export default FailedDialog;
