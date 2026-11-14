package main

// Tidy: paperless-ngx metadata cleanup (fork-only). This file holds the
// paperless-ngx API calls the Tidy page needs. It talks to paperless-ngx
// through PaperlessClient.Do, so it shares the token and TLS settings.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// tidyKinds are the paperless-ngx object types the Tidy page manages, by their
// API path segment.
var tidyKinds = []string{"tags", "correspondents", "document_types", "storage_paths", "custom_fields"}

// tidyDocField is the document field that holds a reference to each kind.
var tidyDocField = map[string]string{
	"tags":           "tags",
	"correspondents": "correspondent",
	"document_types": "document_type",
	"storage_paths":  "storage_path",
	"custom_fields":  "custom_fields",
}

func tidyValidKind(kind string) bool {
	for _, k := range tidyKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// tidyMergeable reports whether objects of a kind can be merged. Custom fields
// cannot: their values differ per document.
func tidyMergeable(kind string) bool {
	return kind == "tags" || kind == "correspondents" || kind == "document_types" || kind == "storage_paths"
}

// tidyHTTP is the part of PaperlessClient the Tidy code needs.
type tidyHTTP interface {
	Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error)
}

type tidyPermSet struct {
	Users  []int `json:"users"`
	Groups []int `json:"groups"`
}

type tidyPerms struct {
	View   tidyPermSet `json:"view"`
	Change tidyPermSet `json:"change"`
}

// tidyObject is one tag, correspondent, document type, storage path or custom
// field as paperless-ngx returns it. Raw keeps the full JSON so a deleted
// object can be recreated with everything it had.
type tidyObject struct {
	ID                int             `json:"id"`
	Name              string          `json:"name"`
	Match             string          `json:"match"`
	MatchingAlgorithm *int            `json:"matching_algorithm"`
	IsInsensitive     *bool           `json:"is_insensitive"`
	Owner             *int            `json:"owner"`
	Permissions       *tidyPerms      `json:"permissions"`
	Parent            *int            `json:"parent"`
	IsInboxTag        bool            `json:"is_inbox_tag"`
	DataType          string          `json:"data_type"`
	Raw               json.RawMessage `json:"-"`
}

// tidyDoc is the slice of a document the Tidy page reads.
type tidyDoc struct {
	ID            int    `json:"id"`
	Created       string `json:"created"`
	Added         string `json:"added"`
	Tags          []int  `json:"tags"`
	Correspondent *int   `json:"correspondent"`
	DocumentType  *int   `json:"document_type"`
	StoragePath   *int   `json:"storage_path"`
	CustomFields  []struct {
		Field int `json:"field"`
	} `json:"custom_fields"`
}

// refs returns the ids of objects of kind this document points at.
func (d tidyDoc) refs(kind string) []int {
	switch kind {
	case "tags":
		return d.Tags
	case "correspondents":
		if d.Correspondent != nil {
			return []int{*d.Correspondent}
		}
	case "document_types":
		if d.DocumentType != nil {
			return []int{*d.DocumentType}
		}
	case "storage_paths":
		if d.StoragePath != nil {
			return []int{*d.StoragePath}
		}
	case "custom_fields":
		ids := make([]int, 0, len(d.CustomFields))
		for _, cf := range d.CustomFields {
			ids = append(ids, cf.Field)
		}
		return ids
	}
	return nil
}

type tidyUser struct {
	ID          int    `json:"id"`
	Username    string `json:"username"`
	Groups      []int  `json:"groups"`
	IsSuperuser bool   `json:"is_superuser"`
}

// tidyAPIError is a non-2xx answer from paperless-ngx.
type tidyAPIError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *tidyAPIError) Error() string {
	body := e.Body
	if len(body) > 300 {
		body = body[:300] + "…"
	}
	return fmt.Sprintf("paperless-ngx %s %s: %d %s", e.Method, e.Path, e.Status, body)
}

// tidySend sends one request. body is JSON-encoded when not nil; out, when not
// nil, receives the decoded answer.
func tidySend(ctx context.Context, c tidyHTTP, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	resp, err := c.Do(ctx, method, path, reader)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &tidyAPIError{Method: method, Path: path, Status: resp.StatusCode, Body: string(raw)}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decoding %s %s: %w", method, path, err)
		}
	}
	return nil
}

// tidyNextPath turns a paginated answer's absolute `next` URL into a path the
// client accepts.
func tidyNextPath(next string) string {
	if next == "" {
		return ""
	}
	if u, err := url.Parse(next); err == nil && u.IsAbs() {
		p := strings.TrimPrefix(u.Path, "/")
		if u.RawQuery != "" {
			p += "?" + u.RawQuery
		}
		return p
	}
	return strings.TrimPrefix(next, "/")
}

// tidyListAll follows paperless-ngx pagination and returns every result.
func tidyListAll(ctx context.Context, c tidyHTTP, path string) ([]json.RawMessage, error) {
	var all []json.RawMessage
	for path != "" {
		var page struct {
			Next    string            `json:"next"`
			Results []json.RawMessage `json:"results"`
		}
		if err := tidySend(ctx, c, "GET", path, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Results...)
		path = tidyNextPath(page.Next)
	}
	return all, nil
}

func tidyDecodeObjects(raws []json.RawMessage) ([]tidyObject, error) {
	objs := make([]tidyObject, 0, len(raws))
	for _, raw := range raws {
		var o tidyObject
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, err
		}
		o.Raw = raw
		objs = append(objs, o)
	}
	return objs, nil
}

// tidyFetchObjects returns every object of a kind, with owner and permissions.
func tidyFetchObjects(ctx context.Context, c tidyHTTP, kind string) ([]tidyObject, error) {
	raws, err := tidyListAll(ctx, c, fmt.Sprintf("api/%s/?page_size=1000&full_perms=true", kind))
	if err != nil {
		return nil, err
	}
	return tidyDecodeObjects(raws)
}

// tidyFetchObject returns one object, or nil when it does not exist.
func tidyFetchObject(ctx context.Context, c tidyHTTP, kind string, id int) (*tidyObject, error) {
	var raw json.RawMessage
	err := tidySend(ctx, c, "GET", fmt.Sprintf("api/%s/%d/?full_perms=true", kind, id), nil, &raw)
	if apiErr, ok := err.(*tidyAPIError); ok && apiErr.Status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	objs, err := tidyDecodeObjects([]json.RawMessage{raw})
	if err != nil {
		return nil, err
	}
	return &objs[0], nil
}

const tidyDocFields = "id,created,added,tags,correspondent,document_type,storage_path,custom_fields"

// tidyFetchDocs lists documents (root documents only: paperless-ngx 3.0 leaves
// file-version rows out of the list) matching the given filter query, e.g.
// "correspondent__id__in=4". An empty filter lists them all.
func tidyFetchDocs(ctx context.Context, c tidyHTTP, filter string) ([]tidyDoc, error) {
	path := "api/documents/?page_size=1000&ordering=id&fields=" + tidyDocFields
	if filter != "" {
		path += "&" + filter
	}
	raws, err := tidyListAll(ctx, c, path)
	if err != nil {
		return nil, err
	}
	docs := make([]tidyDoc, 0, len(raws))
	for _, raw := range raws {
		var d tidyDoc
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	return docs, nil
}

// tidyDocFilter is the document-list filter for "documents that use object id".
func tidyDocFilter(kind string, id int) string {
	switch kind {
	case "tags":
		return fmt.Sprintf("tags__id__all=%d", id)
	case "custom_fields":
		return fmt.Sprintf("custom_fields__id__all=%d", id)
	default:
		return fmt.Sprintf("%s__id__in=%d", tidyDocField[kind], id)
	}
}

// tidyDocsByIDs re-reads the given documents.
func tidyDocsByIDs(ctx context.Context, c tidyHTTP, ids []int) ([]tidyDoc, error) {
	var out []tidyDoc
	for start := 0; start < len(ids); start += 200 {
		end := start + 200
		if end > len(ids) {
			end = len(ids)
		}
		docs, err := tidyFetchDocs(ctx, c, "id__in="+tidyJoinInts(ids[start:end]))
		if err != nil {
			return nil, err
		}
		out = append(out, docs...)
	}
	return out, nil
}

func tidyJoinInts(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ",")
}

// tidyBulkEdit runs one documents/bulk_edit method over ids, in chunks.
// paperless-ngx sends Document Updated for every affected document, so its
// Document Updated workflows run on them.
func tidyBulkEdit(ctx context.Context, c tidyHTTP, ids []int, method string, params map[string]any) error {
	for start := 0; start < len(ids); start += 500 {
		end := start + 500
		if end > len(ids) {
			end = len(ids)
		}
		body := map[string]any{"documents": ids[start:end], "method": method, "parameters": params}
		if err := tidySend(ctx, c, "POST", "api/documents/bulk_edit/", body, nil); err != nil {
			return err
		}
	}
	return nil
}

// tidySetDocRef points documents at object id of a single-valued kind.
func tidySetDocRef(ctx context.Context, c tidyHTTP, kind string, ids []int, id int) error {
	field := tidyDocField[kind]
	return tidyBulkEdit(ctx, c, ids, "set_"+field, map[string]any{field: id})
}

// tidyFetchUser finds a paperless-ngx user by name.
func tidyFetchUser(ctx context.Context, c tidyHTTP, username string) (*tidyUser, error) {
	var page struct {
		Results []tidyUser `json:"results"`
	}
	if err := tidySend(ctx, c, "GET", "api/users/?username__iexact="+url.QueryEscape(username), nil, &page); err != nil {
		return nil, err
	}
	for _, u := range page.Results {
		if strings.EqualFold(u.Username, username) {
			u := u
			return &u, nil
		}
	}
	return nil, nil
}

// tidyFetchUsers maps user ids to names, for the owner column. A token that
// may not list users gets an empty map.
func tidyFetchUsers(ctx context.Context, c tidyHTTP) map[int]string {
	names := map[int]string{}
	raws, err := tidyListAll(ctx, c, "api/users/?page_size=1000")
	if err != nil {
		log.Debugf("tidy: listing users failed: %v", err)
		return names
	}
	for _, raw := range raws {
		var u tidyUser
		if json.Unmarshal(raw, &u) == nil {
			names[u.ID] = u.Username
		}
	}
	return names
}

// tidyCreateBody builds the POST body that recreates an object from the JSON
// paperless-ngx returned for it, including matching rules, owner and
// permissions. Read-only fields are left out.
func tidyCreateBody(kind string, raw json.RawMessage) (map[string]any, error) {
	var src map[string]any
	if err := json.Unmarshal(raw, &src); err != nil {
		return nil, err
	}
	keep := []string{"name", "match", "matching_algorithm", "is_insensitive", "owner"}
	switch kind {
	case "tags":
		keep = append(keep, "color", "is_inbox_tag", "parent")
	case "storage_paths":
		keep = append(keep, "path")
	case "custom_fields":
		keep = []string{"name", "data_type", "extra_data"}
	}
	body := map[string]any{}
	for _, k := range keep {
		if v, ok := src[k]; ok {
			body[k] = v
		}
	}
	if perms, ok := src["permissions"]; ok && perms != nil && kind != "custom_fields" {
		body["set_permissions"] = perms
	}
	return body, nil
}

// tidyUniqueSorted returns ids sorted, without duplicates.
func tidyUniqueSorted(ids []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}
