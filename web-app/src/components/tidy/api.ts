// Fork-only: types and calls for the Tidy page (Go side: tidy_*.go).

export type Kind = "correspondents" | "tags" | "document_types" | "storage_paths" | "custom_fields";

export const KINDS: { kind: Kind; label: string; singular: string }[] = [
  { kind: "correspondents", label: "Correspondents", singular: "correspondent" },
  { kind: "tags", label: "Tags", singular: "tag" },
  { kind: "document_types", label: "Document types", singular: "document type" },
  { kind: "storage_paths", label: "Storage paths", singular: "storage path" },
  { kind: "custom_fields", label: "Custom fields", singular: "custom field" },
];

export const MATCH_NAMES: Record<number, string> = {
  0: "None",
  1: "Any word",
  2: "All words",
  3: "Exact match",
  4: "Regular expression",
  5: "Fuzzy word",
  6: "Automatic",
};

export interface Ref {
  kind: Kind;
  object_id: number;
  name?: string;
  source: string;
  source_id?: string;
  label: string;
  by_name?: boolean;
}

export interface Row {
  id: number;
  name: string;
  doc_count: number;
  last_created?: string;
  last_added?: string;
  matching_algorithm?: number;
  match: string;
  owner: number | null;
  owner_name?: string;
  parent?: number;
  is_inbox_tag?: boolean;
  data_type?: string;
  visible_to_viewer?: boolean;
  references: Ref[];
}

export type FindingType =
  | "unused"
  | "near_duplicate"
  | "dead_rule"
  | "ignored_rule_text"
  | "hidden_from_viewer"
  | "stale_reference";

export interface Finding {
  kind: Kind;
  type: FindingType;
  ids: number[];
  detail: string;
}

export interface WorkflowInfo {
  id: number;
  name: string;
  enabled: boolean;
}

export interface Alias {
  id: number;
  created_at: string;
  kind: Kind;
  name: string;
  target_id: number;
  action_id: number;
}

export interface Inventory {
  generated_at: string;
  writes: boolean;
  viewer: string;
  viewer_id: number;
  viewer_found: boolean;
  document_count: number;
  objects: Record<Kind, Row[]>;
  findings: Finding[];
  matching_summary: Record<string, Record<string, number>>;
  updated_workflows: WorkflowInfo[];
  aliases: Alias[];
  warnings: string[] | null;
  hygiene: HygieneItem[] | null;
  field_audit: FieldAudit[] | null;
}

export interface HygieneItem {
  key: string;
  label: string;
  count: number;
  /** paperless-ngx documents-list query showing these documents */
  query?: string;
}

export interface ValueCount {
  value: string;
  count: number;
}

export interface FieldAudit {
  id: number;
  name: string;
  data_type: string;
  doc_count: number;
  empty_count: number;
  distinct: number;
  top: ValueCount[];
  variants: ValueCount[][];
  unused_options: string[];
  no_currency?: number;
}

export interface SplitRun {
  id: number;
  created_at: string;
  updated_at: string;
  correspondent_id: number;
  correspondent_name: string;
  exclude_self: boolean;
  status: "running" | "done" | "stopped" | "failed";
  total: number;
  done: number;
  error?: string;
}

export interface SplitDoc {
  id: number;
  title: string;
  moved: boolean;
}

export interface SplitGroup {
  key: string;
  target_id?: number;
  target_name: string;
  new: boolean;
  stay: boolean;
  similar?: string;
  docs: SplitDoc[];
}

export interface SplitView {
  run: SplitRun;
  groups: SplitGroup[];
  failed: { doc_id: number; title: string; error?: string }[];
}

export interface FillRun {
  id: number;
  created_at: string;
  status: "running" | "done" | "stopped";
  total: number;
  done: number;
  calls: number;
}

export interface FillTag {
  id: number;
  name: string;
}

export interface FillItem {
  doc_id: number;
  title: string;
  ask_tags: boolean;
  tags: FillTag[];
  tag_names: string[];
  tag_ids: number[];
  unknown_tags: string[];
  has_tags_now: boolean;
  ask_type: boolean;
  type_name?: string;
  type_id?: number;
  has_type_now: boolean;
  ignored: boolean;
  error?: string;
}

export interface FillIgnored {
  doc_id: number;
  title: string;
  created_at: string;
  /** What the document lacks today; both false once it has been filled since. */
  no_tags: boolean;
  no_type: boolean;
  /** paperless-ngx no longer lists it. */
  gone: boolean;
}

/** What a run would ask about now, and the documents Fill leaves alone. */
export interface FillStatus {
  documents: number;
  without_tags: number;
  without_type: number;
  calls: number;
  ignored: FillIgnored[];
}

export interface TitleRun {
  id: number;
  created_at: string;
  updated_at: string;
  status: "running" | "done" | "stopped";
  total: number;
  done: number;
}

export interface TitleItem {
  doc_id: number;
  /** The title when the run asked. */
  title: string;
  /** The title now. */
  current: string;
  suggested: string;
  /** The title changed since the run: applied, or edited by hand. */
  changed: boolean;
  gone: boolean;
  ignored: boolean;
  error?: string;
}

export interface TitleView {
  run: TitleRun;
  items: TitleItem[];
}

export interface TitleIgnored {
  doc_id: number;
  title: string;
  created_at: string;
  current: string;
  gone: boolean;
}

export type DateRule = "reingest" | "after_arrival";

export interface DateItem {
  doc_id: number;
  title: string;
  created: string;
  added: string;
  rule: DateRule;
  /** after_arrival: days the created date lies after the added day */
  days?: number;
  /** reingest: split, merge or edit, the documents it was made from, and the added date it should carry */
  origin?: string;
  source_ids?: number[];
  source_added?: string;
  /**
   * where that date was read: document, trash, remembered once the source is gone, or
   * estimated when the source was gone before it was seen (then it is the added date of
   * estimated_from, the next document added after the source)
   */
  source_state?: string;
  estimated_from?: number;
}

export interface DateIgnored {
  doc_id: number;
  rule: DateRule;
  title: string;
  created_at: string;
  current: string;
  gone: boolean;
  /** the rule would still list the document today */
  flagged: boolean;
}

export interface DateChecks {
  items: DateItem[];
  ignored: DateIgnored[];
  /** split or merged documents whose source is gone, was never seen and cannot be dated */
  unchecked: number[];
  fix_script?: string;
  warnings: string[];
}

export interface TitleStatus {
  candidates: number;
  documents: number;
  ignored: TitleIgnored[];
}

export interface FillView {
  run: FillRun;
  items: FillItem[];
}

export interface FillApplyItem {
  doc_id: number;
  tag_ids: number[];
  type_id: number;
}

export interface SplitApplyGroup {
  doc_ids: number[];
  target_id: number;
  new_name: string;
}

export interface Action {
  id: number;
  created_at: string;
  action: string;
  kind: Kind;
  object_id: number;
  object_name: string;
  target_id?: number;
  target_name?: string;
  summary: string;
  status: string;
  error?: string;
  undone: boolean;
  undone_at?: string;
  undo_note?: string;
}

export interface MergePlan {
  kind: Kind;
  source: Row;
  target: Row;
  docs: number[] | null;
  children?: number[];
  references: Ref[];
  updated_workflows: WorkflowInfo[] | null;
  alias?: string;
  blocked?: string;
}

export interface LLMGroup {
  ids: number[];
  keep: number;
  reason: string;
}

/** An error from the Tidy API, with the refusal details when there are any. */
export class TidyError extends Error {
  references?: Ref[];
  driftedDocs?: number[];
  status: number;
  constructor(message: string, status: number, body?: { references?: Ref[]; drifted_docs?: number[] }) {
    super(message);
    this.status = status;
    this.references = body?.references;
    this.driftedDocs = body?.drifted_docs;
  }
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`./api/tidy/${path}`, {
    method,
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let data: unknown = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = null;
  }
  if (!res.ok) {
    const d = (data ?? {}) as { error?: string; references?: Ref[]; drifted_docs?: number[] };
    throw new TidyError(d.error || text || `HTTP ${res.status}`, res.status, d);
  }
  return data as T;
}

export const tidyApi = {
  inventory: (refresh = false) => call<Inventory>("GET", `inventory${refresh ? "?refresh=1" : ""}`),
  actions: () => call<Action[]>("GET", "actions"),
  previewMerge: (kind: Kind, source_id: number, target_id: number) =>
    call<MergePlan>("POST", "merge/preview", { kind, source_id, target_id }),
  merge: (kind: Kind, source_id: number, target_id: number) =>
    call<Action>("POST", "merge", { kind, source_id, target_id }),
  rename: (kind: Kind, id: number, name: string) => call<Action>("POST", "rename", { kind, id, name }),
  remove: (kind: Kind, id: number) => call<Action>("POST", "delete", { kind, id }),
  removeMany: (kind: Kind, ids: number[]) => call<Action>("POST", "delete-many", { kind, ids }),
  setMatching: (kind: Kind, ids: number[], matching_algorithm: number, match?: string) =>
    call<Action>("POST", "matching", { kind, ids, matching_algorithm, match }),
  grantViewer: (kind: Kind, ids: number[]) => call<Action>("POST", "grant-viewer", { kind, ids }),
  undo: (id: number, force = false) => call<Action>("POST", `actions/${id}/undo`, { force }),
  deleteAlias: (id: number) => call<unknown>("DELETE", `aliases/${id}`),
  llmGroups: (kind: Kind) => call<LLMGroup[]>("POST", "llm-groups", { kind }),
  splitStart: (correspondent_id: number) => call<SplitRun>("POST", "split/start", { correspondent_id }),
  fillStart: (limit: number) => call<FillRun>("POST", "fill/start", { limit }),
  fillRuns: () => call<FillRun[]>("GET", "fill/runs"),
  fillView: (id: number) => call<FillView>("GET", `fill/runs/${id}`),
  fillStop: () => call<{ stopping: boolean }>("POST", "fill/stop"),
  fillApply: (run_id: number, items: FillApplyItem[]) => call<Action>("POST", "fill/apply", { run_id, items }),
  fillStatus: () => call<FillStatus>("GET", "fill/status"),
  titlesStatus: () => call<TitleStatus>("GET", "titles/status"),
  titlesStart: (limit: number, doc_ids: number[] = []) =>
    call<TitleRun>("POST", "titles/start", { limit, doc_ids }),
  titlesStop: () => call<{ stopping: boolean }>("POST", "titles/stop"),
  titlesRuns: () => call<TitleRun[]>("GET", "titles/runs"),
  titlesView: (id: number) => call<TitleView>("GET", `titles/runs/${id}`),
  titlesIgnore: (items: { doc_id: number; title: string }[]) =>
    call<{ ignored: number }>("POST", "titles/ignore", { items }),
  titlesUnignore: (doc_ids: number[]) => call<{ unignored: number }>("POST", "titles/unignore", { doc_ids }),
  titlesApply: (run_id: number, items: { doc_id: number; title: string }[]) =>
    call<Action>("POST", "titles/apply", { run_id, items }),
  dates: () => call<DateChecks>("GET", "dates"),
  datesIgnore: (items: { doc_id: number; rule: DateRule; title: string }[]) =>
    call<{ ignored: number }>("POST", "dates/ignore", { items }),
  datesUnignore: (items: { doc_id: number; rule: DateRule }[]) =>
    call<{ unignored: number }>("POST", "dates/unignore", { items }),
  fillIgnore: (items: { doc_id: number; title: string }[]) =>
    call<{ ignored: number }>("POST", "fill/ignore", { items }),
  fillUnignore: (doc_ids: number[]) => call<{ unignored: number }>("POST", "fill/unignore", { doc_ids }),
  splitRuns: () => call<SplitRun[]>("GET", "split/runs"),
  splitView: (id: number) => call<SplitView>("GET", `split/runs/${id}`),
  splitStop: () => call<{ stopping: boolean }>("POST", "split/stop"),
  splitApply: (run_id: number, groups: SplitApplyGroup[]) => call<Action>("POST", "split/apply", { run_id, groups }),
};

/** The paperless-ngx documents list filtered to one object. */
export function docsLink(base: string, kind: Kind, id: number): string {
  const filter: Record<Kind, string> = {
    correspondents: `correspondent__id__in=${id}`,
    tags: `tags__id__all=${id}`,
    document_types: `document_type__id__in=${id}`,
    storage_paths: `storage_path__id__in=${id}`,
    custom_fields: `custom_fields__id__all=${id}`,
  };
  return `${base}/documents?${filter[kind]}`;
}

/**
 * paperless-ngx's own page for a kind of object. paperless-ngx 3.0 has no URL for one
 * object's edit dialog, only for its list (Attributes, one section per kind).
 */
export function attributesLink(base: string, kind: Kind): string {
  const section: Record<Kind, string> = {
    correspondents: "correspondents",
    tags: "tags",
    document_types: "documenttypes",
    storage_paths: "storagepaths",
    custom_fields: "customfields",
  };
  return `${base}/attributes/${section[kind]}`;
}
