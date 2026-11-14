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
  setMatching: (kind: Kind, ids: number[], matching_algorithm: number, match?: string) =>
    call<Action>("POST", "matching", { kind, ids, matching_algorithm, match }),
  grantViewer: (kind: Kind, ids: number[]) => call<Action>("POST", "grant-viewer", { kind, ids }),
  undo: (id: number, force = false) => call<Action>("POST", `actions/${id}/undo`, { force }),
  deleteAlias: (id: number) => call<unknown>("DELETE", `aliases/${id}`),
  llmGroups: (kind: Kind) => call<LLMGroup[]>("POST", "llm-groups", { kind }),
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
