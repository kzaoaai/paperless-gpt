package main

import (
	"context"
	"fmt"
	"strings"
)

// saveVersionText writes the recognized text into a document whose searchable
// PDF paperless-ngx has just added as a new version. paperless-ngx fills a new
// version's content with its own extraction of that PDF, which misplaces signs
// and spaces next to numbers in right-to-left text, and on every later update
// rebuilds the document's search index entry from its original's content. So
// the text goes into both: the new version (what paperless-ngx shows, and what
// paperless-gpt reads back) and the original (what search finds). Only content
// is written. The new version's change is recorded in the modification history,
// so it can be undone like any other update.
func (app *App) saveVersionText(ctx context.Context, documentID int, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	current, err := app.Client.GetDocument(ctx, documentID) // the latest version's content
	if err != nil {
		return fmt.Errorf("error reading document %d: %w", documentID, err)
	}
	if current.Content != text {
		if err := app.Client.SetDocumentContent(ctx, documentID, 0, text); err != nil {
			return err
		}
		if err := InsertModification(app.Database, &ModificationHistory{
			DocumentID:    uint(documentID),
			ModField:      "content",
			PreviousValue: current.Content,
			NewValue:      text,
		}); err != nil {
			log.Warnf("Document %d: could not record the content change: %v", documentID, err)
		}
	}
	// Written whether or not it already matches: reading the original's
	// content back would cost a request too, and the write is idempotent.
	return app.Client.SetDocumentContent(ctx, documentID, documentID, text)
}
