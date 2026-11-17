package main

import (
	"context"
	"os"
	"strings"
	"sync"
)

// The fork's viewer user (TIDY_VIEWER_USER, default "mcp-reader") is a
// view-only paperless-ngx user. It sees objects owned by paperless-gpt's API
// user only through per-object view grants. paperless-ngx workflows can grant
// them on documents, but nothing grants them on the correspondents and tags
// paperless-gpt creates while processing, so those stayed hidden from the
// viewer until someone used Tidy's "Grant view". grantViewerOnCreated adds the
// grant right after creating one, the way Tidy's split already does for the
// correspondents it creates.

// viewerUserName is the user Tidy checks visibility for and that new objects
// are granted to.
func viewerUserName() string {
	if v := strings.TrimSpace(os.Getenv("TIDY_VIEWER_USER")); v != "" {
		return v
	}
	return "mcp-reader"
}

// viewerIDs caches the viewer's user id by name. Only found users are cached,
// so a viewer created later is picked up on the next creation.
var viewerIDs sync.Map

// grantViewerOnCreated gives the viewer view permission on a correspondent or
// tag paperless-gpt just created (objectType "correspondents" or "tags"). It
// merges with the object's existing permissions and never fails the caller:
// a missing viewer user or a refused request is only logged.
func (client *PaperlessClient) grantViewerOnCreated(ctx context.Context, objectType string, id int) {
	name := viewerUserName()
	var uid int
	if v, ok := viewerIDs.Load(name); ok {
		uid = v.(int)
	} else {
		u, err := tidyFetchUser(ctx, client, name)
		if err != nil {
			log.Warnf("Could not look up viewer user %q to grant view on new %s %d: %v", name, objectType, id, err)
			return
		}
		if u == nil {
			log.Debugf("No paperless-ngx user %q; new %s %d gets no viewer grant", name, objectType, id)
			return
		}
		uid = u.ID
		viewerIDs.Store(name, uid)
	}
	body := map[string]any{
		"objects":     []int{id},
		"object_type": objectType,
		"operation":   "set_permissions",
		"merge":       true,
		"permissions": tidyPerms{
			View:   tidyPermSet{Users: []int{uid}, Groups: []int{}},
			Change: tidyPermSet{Users: []int{}, Groups: []int{}},
		},
	}
	if err := tidySend(ctx, client, "POST", "api/bulk_edit_objects/", body, nil); err != nil {
		log.Warnf("Could not grant %s view on new %s %d: %v", name, objectType, id, err)
		return
	}
	log.Infof("Granted %s view on new %s %d", name, objectType, id)
}
