package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A correspondent or tag paperless-gpt creates is granted view access to the
// viewer user, merged with its existing permissions.
func TestGrantViewerOnCreated(t *testing.T) {
	for _, tc := range []struct {
		objectType string
		create     func(ctx context.Context, c *PaperlessClient) (int, error)
		path       string
	}{
		{"correspondents", func(ctx context.Context, c *PaperlessClient) (int, error) {
			return c.CreateOrGetCorrespondent(ctx, instantiateCorrespondent("New Sender"))
		}, "/api/correspondents/"},
		{"tags", func(ctx context.Context, c *PaperlessClient) (int, error) {
			return c.CreateTag(ctx, "New Tag")
		}, "/api/tags/"},
	} {
		t.Run(tc.objectType, func(t *testing.T) {
			viewerIDs.Delete("viewer-x")
			t.Setenv("TIDY_VIEWER_USER", "viewer-x")
			env := newTestEnv(t)
			defer env.teardown()

			env.setMockResponse(tc.path, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.Write([]byte(`{"results": []}`))
					return
				}
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{"id": 77}`))
			})
			env.setMockResponse("/api/users/", func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "viewer-x", r.URL.Query().Get("username__iexact"))
				w.Write([]byte(`{"results": [{"id": 6, "username": "viewer-x"}]}`))
			})
			var got map[string]any
			env.setMockResponse("/api/bulk_edit_objects/", func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				require.NoError(t, json.Unmarshal(b, &got))
				w.Write([]byte(`{"result": "OK"}`))
			})

			id, err := tc.create(context.Background(), env.client)
			require.NoError(t, err)
			assert.Equal(t, 77, id)
			require.NotNil(t, got, "no set_permissions request")
			assert.Equal(t, tc.objectType, got["object_type"])
			assert.Equal(t, "set_permissions", got["operation"])
			assert.Equal(t, true, got["merge"])
			assert.Equal(t, []any{float64(77)}, got["objects"])
			perms := got["permissions"].(map[string]any)
			assert.Equal(t, []any{float64(6)}, perms["view"].(map[string]any)["users"])
			assert.Equal(t, []any{}, perms["change"].(map[string]any)["users"])
		})
	}
}

// Without a viewer user, creation works and sends no grant.
func TestGrantViewerOnCreatedNoViewer(t *testing.T) {
	viewerIDs.Delete("nobody")
	t.Setenv("TIDY_VIEWER_USER", "nobody")
	env := newTestEnv(t)
	defer env.teardown()
	env.setMockResponse("/api/tags/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id": 5}`))
	})
	id, err := env.client.CreateTag(context.Background(), "Other")
	require.NoError(t, err)
	assert.Equal(t, 5, id)
	_, granted := env.mockResponses["/api/bulk_edit_objects/"]
	assert.False(t, granted)
}
