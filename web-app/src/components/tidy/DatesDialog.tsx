import React, { useEffect, useRef, useState } from "react";
import Button from "../ui/Button";
import Modal from "../ui/Modal";
import { DateChecks, DateItem, DateRule, tidyApi } from "./api";

interface DatesDialogProps {
  paperlessUrl: string;
  /** dirty: the ignore list changed, so the page's counts are stale */
  onClose: (dirty: boolean) => void;
}

const RULE_LABEL: Record<DateRule, string> = {
  reingest: "split or merged",
  after_arrival: "dated after it arrived",
};

/** The day part of a paperless-ngx date or timestamp, as the server wrote it. */
const day = (s: string | undefined) => (s ?? "").slice(0, 10);

const madeFrom = (i: DateItem) => {
  const ids = (i.source_ids ?? []).map((id) => `#${id}`).join(", ");
  const where =
    i.source_state === "trash"
      ? " (in the trash)"
      : i.source_state === "remembered"
        ? " (gone since)"
        : i.source_state === "estimated"
          ? " (gone)"
          : "";
  return `${i.origin} of ${ids}${where}`;
};

const estimated = (i: DateItem) => i.source_state === "estimated";

/**
 * Document dates: documents paperless-ngx made by splitting or merging that
 * still carry the day of that change as their added date, and documents whose
 * own date is later than the day they arrived. Read-only: the added date can
 * only be set in the paperless-ngx shell, so that list comes with the script.
 */
const DatesDialog: React.FC<DatesDialogProps> = ({ paperlessUrl, onClose }) => {
  const [checks, setChecks] = useState<DateChecks | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [showIgnored, setShowIgnored] = useState(false);
  const [copied, setCopied] = useState("");
  const dirty = useRef(false);
  const script = useRef<HTMLTextAreaElement>(null);

  const fail = (e: unknown) => setError(e instanceof Error ? e.message : String(e));
  const reload = () => tidyApi.dates().then(setChecks).catch(fail);

  useEffect(() => {
    tidyApi
      .dates()
      .then(setChecks)
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, []);

  const change = async (run: () => Promise<unknown>) => {
    setBusy(true);
    setError(null);
    try {
      await run();
      dirty.current = true;
      await reload();
    } catch (e) {
      fail(e);
    } finally {
      setBusy(false);
    }
  };
  const ignore = (i: DateItem) => change(() => tidyApi.datesIgnore([{ doc_id: i.doc_id, rule: i.rule, title: i.title }]));

  // The clipboard API needs HTTPS; over plain HTTP the script is selected instead.
  const copy = async () => {
    const text = checks?.fix_script ?? "";
    try {
      await navigator.clipboard.writeText(text);
      setCopied("Copied.");
    } catch {
      script.current?.focus();
      script.current?.select();
      setCopied("Selected: copy it with ⌘C or Ctrl+C.");
    }
  };

  const link = (id: number) => (paperlessUrl ? `${paperlessUrl}/documents/${id}/details` : undefined);
  const idCell = (id: number) =>
    link(id) ? (
      <a className="text-primary hover:underline" href={link(id)} target="_blank" rel="noreferrer">
        {id}
      </a>
    ) : (
      id
    );
  const reingest = (checks?.items ?? []).filter((i) => i.rule === "reingest");
  const after = (checks?.items ?? []).filter((i) => i.rule === "after_arrival");
  const ignored = checks?.ignored ?? [];
  const unchecked = checks?.unchecked ?? [];
  const th = "px-2 py-2 text-left font-medium";

  return (
    <Modal open onClose={busy ? () => {} : () => onClose(dirty.current)} title="Document dates" size="focus">
      <div className="flex min-h-0 flex-1 flex-col">
        <div className="flex-1 space-y-6 overflow-y-auto px-6 py-4 text-sm">
          <p className="text-muted">
            A created date that differs from the added date is normal: most of an archive is old paper scanned later.
            These two lists are the cases where a date is likely wrong. Nothing here changes paperless-ngx. An ignored
            document leaves its list until you un-ignore it.
          </p>
          {!checks && !error && <p className="text-muted">Loading…</p>}
          {(checks?.warnings ?? []).map((w) => (
            <p key={w} className="text-warn">
              {w}
            </p>
          ))}

          {checks && (
            <section className="space-y-2">
              <h3 className="font-semibold">
                Split or merged, added date not the original's <span className="font-normal text-muted">({reingest.length})</span>
              </h3>
              <p className="text-muted">
                paperless-ngx adds the pieces of a split or a merge as new documents, dated the day of the change, so a
                document you have had for years shows up as your latest. Listed when the piece was added more than a
                day after the document it came from.
              </p>
              {reingest.length === 0 ? (
                <p className="text-faint">None.</p>
              ) : (
                <>
                  <div className="overflow-x-auto rounded-md border border-line">
                    <table className="w-full text-sm">
                      <thead className="border-b border-line bg-surface-2 text-xs text-muted">
                        <tr>
                          <th className={th}>ID</th>
                          <th className={th}>Title</th>
                          <th className={th}>Added</th>
                          <th className={th}>Made from</th>
                          <th className={th}>Should be</th>
                          <th className={th} />
                        </tr>
                      </thead>
                      <tbody className="divide-y divide-line">
                        {reingest.map((i) => (
                          <tr key={i.doc_id}>
                            <td className="whitespace-nowrap px-2 py-1.5 tabular-nums">{idCell(i.doc_id)}</td>
                            <td className="px-2 py-1.5">{i.title}</td>
                            <td className="whitespace-nowrap px-2 py-1.5 tabular-nums">{day(i.added)}</td>
                            <td className="px-2 py-1.5">{madeFrom(i)}</td>
                            <td className="whitespace-nowrap px-2 py-1.5 tabular-nums">
                              {estimated(i) ? (
                                <>
                                  about {day(i.source_added)}{" "}
                                  <span className="text-xs text-faint">(from #{i.estimated_from})</span>
                                </>
                              ) : (
                                day(i.source_added)
                              )}
                            </td>
                            <td className="px-2 py-1 text-right">
                              <Button size="sm" variant="ghost" onClick={() => ignore(i)} disabled={busy}>
                                Ignore
                              </Button>
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                  {reingest.some(estimated) && (
                    <p className="text-muted">
                      "About" marks an estimate. The document it was made from is gone and Tidy never saw it, so the
                      date is that of the next document added after it: paperless-ngx numbers documents in the order
                      they arrive, so the original arrived no later than that.
                    </p>
                  )}
                  <p className="text-muted">
                    paperless-ngx has no field or API call for the added date, so the fix is this script, run in its
                    shell. It gives each listed document the date in "Should be", then refreshes the search index,
                    which also runs paperless-ngx's Document Updated workflows on them.
                  </p>
                  <textarea
                    ref={script}
                    readOnly
                    aria-label="Script that sets the added dates"
                    className="h-44 w-full rounded-md border border-line bg-surface-2 p-2 font-mono text-xs"
                    value={checks.fix_script ?? ""}
                  />
                  <div className="flex items-center gap-3">
                    <Button size="sm" onClick={copy}>
                      Copy the script
                    </Button>
                    {copied && <span className="text-xs text-muted">{copied}</span>}
                  </div>
                </>
              )}
              {unchecked.length > 0 && (
                <p className="text-xs text-faint">
                  Not checked, because the document they were made from no longer exists and nothing was added
                  between it and them to date it by: {unchecked.join(", ")}.
                </p>
              )}
            </section>
          )}

          {checks && (
            <section className="space-y-2">
              <h3 className="font-semibold">
                Dated after they arrived <span className="font-normal text-muted">({after.length})</span>
              </h3>
              <p className="text-muted">
                The created date is the document's own date, so it cannot be later than the day the document was
                added. These are two days or more after it: usually a due date, an expiry date or a period end picked
                as the document's date. Open one to correct it in paperless-ngx.
              </p>
              {after.length === 0 ? (
                <p className="text-faint">None.</p>
              ) : (
                <div className="overflow-x-auto rounded-md border border-line">
                  <table className="w-full text-sm">
                    <thead className="border-b border-line bg-surface-2 text-xs text-muted">
                      <tr>
                        <th className={th}>ID</th>
                        <th className={th}>Title</th>
                        <th className={th}>Created</th>
                        <th className={th}>Added</th>
                        <th className={th}>Days after</th>
                        <th className={th} />
                      </tr>
                    </thead>
                    <tbody className="divide-y divide-line">
                      {after.map((i) => (
                        <tr key={i.doc_id}>
                          <td className="whitespace-nowrap px-2 py-1.5 tabular-nums">{idCell(i.doc_id)}</td>
                          <td className="px-2 py-1.5">{i.title}</td>
                          <td className="whitespace-nowrap px-2 py-1.5 tabular-nums">{day(i.created)}</td>
                          <td className="whitespace-nowrap px-2 py-1.5 tabular-nums">{day(i.added)}</td>
                          <td className="px-2 py-1.5 tabular-nums">{i.days}</td>
                          <td className="px-2 py-1 text-right">
                            <Button size="sm" variant="ghost" onClick={() => ignore(i)} disabled={busy}>
                              Ignore
                            </Button>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </section>
          )}

          {ignored.length > 0 && (
            <section className="space-y-2">
              <Button size="sm" variant="ghost" onClick={() => setShowIgnored(!showIgnored)}>
                {showIgnored ? "Hide" : "Show"} {ignored.length} ignored
              </Button>
              {showIgnored && (
                <ul className="divide-y divide-line rounded-md border border-line">
                  {ignored.map((d) => (
                    <li key={`${d.rule}:${d.doc_id}`} className="flex items-center justify-between gap-3 px-2 py-1.5">
                      <span>
                        <span className="mr-2 tabular-nums">{idCell(d.doc_id)}</span>
                        {d.gone ? <span className="text-faint">gone from paperless-ngx</span> : d.current}
                        <span className="ml-2 text-xs text-faint">
                          {RULE_LABEL[d.rule]}
                          {!d.flagged && !d.gone && ", fixed since"}
                        </span>
                      </span>
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={busy}
                        onClick={() => change(() => tidyApi.datesUnignore([{ doc_id: d.doc_id, rule: d.rule }]))}
                      >
                        Un-ignore
                      </Button>
                    </li>
                  ))}
                </ul>
              )}
            </section>
          )}
          {error && <p className="text-neg">{error}</p>}
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

export default DatesDialog;
