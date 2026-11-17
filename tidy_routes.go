package main

// Tidy: HTTP routes. registerTidy is the only call main.go makes, so an
// upstream sync conflicts on that one line at most.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"

	"paperless-gpt/sanitize"

	"github.com/gin-gonic/gin"
	"github.com/tmc/langchaingo/llms"
)

// registerTidy adds the Tidy page and its API.
func registerTidy(router *gin.Engine, api *gin.RouterGroup, app *App) {
	client, ok := app.Client.(tidyHTTP)
	if !ok {
		log.Warn("Tidy page disabled: the paperless-ngx client does not support raw requests")
		return
	}
	if app.Database != nil {
		if err := app.Database.AutoMigrate(&TidyAction{}, &TidyAlias{}, &TidySplitRun{}, &TidyFillRun{}, &TidyFillIgnore{}, &TidyTitleRun{}, &TidyTitleIgnore{}, &TidyDateIgnore{}, &TidyDateSource{}, &ProcessingFailure{}); err != nil {
			log.Errorf("Tidy page disabled: migrating its tables failed: %v", err)
			return
		}
	}
	viewer := viewerUserName()
	s := &tidyService{
		client:     client,
		db:         app.Database,
		writes:     strings.EqualFold(os.Getenv("TIDY_WRITES"), "true"),
		viewer:     viewer,
		configRefs: tidyCurrentConfigRefs,
	}
	if app.LLM != nil {
		s.llm = func(ctx context.Context, prompt string) (string, error) {
			resp, err := app.LLM.GenerateContent(ctx, []llms.MessageContent{
				{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextContent{Text: prompt}}},
			})
			if err != nil {
				return "", err
			}
			if len(resp.Choices) == 0 {
				return "", errors.New("the LLM returned no answer")
			}
			return resp.Choices[0].Content, nil
		}
		// The same prompt, candidate filtering and blacklist as document
		// processing, so a split suggests what processing would.
		s.suggest = func(ctx context.Context, title, content string, names []string) (string, error) {
			content = sanitize.Sanitize(content)
			candidates := filterCorrespondentsForPrompt(names, content, title, correspondentPromptLimit)
			return app.getSuggestedCorrespondent(ctx, content, title, candidates, correspondentBlackList, nil)
		}
		// Only the tag and type generators of document processing, so nothing
		// else about the document is asked or changed.
		s.fillSuggest = func(ctx context.Context, docID int, wantTags, wantType bool) (string, []string, string, error) {
			doc, err := app.Client.GetDocument(ctx, docID)
			if err != nil {
				return "", nil, "", err
			}
			req := GenerateSuggestionsRequest{Documents: []Document{doc}, GenerateTags: wantTags, GenerateDocumentTypes: wantType}
			genCtx, err := app.prepareSuggestionGenerationContext(ctx, req)
			if err != nil {
				return doc.Title, nil, "", err
			}
			sug, err := app.generateSingleDocumentSuggestion(ctx, req, doc, genCtx, log.WithField("tidy", "fill"))
			if err != nil {
				return doc.Title, nil, "", err
			}
			return doc.Title, sug.SuggestedTags, sug.SuggestedDocumentType, nil
		}
		// Only the title generator, so nothing else about the document is asked.
		s.titleSuggest = func(ctx context.Context, docID int) (string, string, error) {
			doc, err := app.Client.GetDocument(ctx, docID)
			if err != nil {
				return "", "", err
			}
			req := GenerateSuggestionsRequest{Documents: []Document{doc}, GenerateTitles: true}
			genCtx, err := app.prepareSuggestionGenerationContext(ctx, req)
			if err != nil {
				return doc.Title, "", err
			}
			sug, err := app.generateSingleDocumentSuggestion(ctx, req, doc, genCtx, log.WithField("tidy", "titles"))
			if err != nil {
				return doc.Title, "", err
			}
			return doc.Title, sug.SuggestedTitle, nil
		}
	}
	// A run left "running" by a restart can never finish.
	if app.Database != nil {
		app.Database.Model(&TidySplitRun{}).Where("status = ?", "running").Update("status", "stopped")
		app.Database.Model(&TidyFillRun{}).Where("status = ?", "running").Update("status", "stopped")
		app.Database.Model(&TidyTitleRun{}).Where("status = ?", "running").Update("status", "stopped")
	}
	s.routes(api.Group("/tidy"))

	if _, err := os.Stat("web-app/dist"); err == nil {
		router.GET("/tidy", func(c *gin.Context) { c.File("web-app/dist/index.html") })
	} else {
		router.GET("/tidy", func(c *gin.Context) { serveEmbeddedFile(c, "", "index.html") })
	}
	log.Infof("Tidy page enabled (writes %v, viewer %q)", s.writes, viewer)
}

func (s *tidyService) routes(g *gin.RouterGroup) {
	g.GET("/inventory", func(c *gin.Context) {
		inv, err := s.inventory(c.Request.Context(), c.Query("refresh") != "")
		tidyRespond(c, inv, err)
	})
	g.POST("/merge/preview", func(c *gin.Context) {
		var req struct {
			Kind     string `json:"kind"`
			SourceID int    `json:"source_id"`
			TargetID int    `json:"target_id"`
		}
		if !tidyBind(c, &req, &req.Kind) {
			return
		}
		plan, _, err := s.planMerge(c.Request.Context(), req.Kind, req.SourceID, req.TargetID)
		tidyRespond(c, plan, err)
	})
	g.POST("/merge", func(c *gin.Context) {
		var req struct {
			Kind     string `json:"kind"`
			SourceID int    `json:"source_id"`
			TargetID int    `json:"target_id"`
		}
		if !tidyBind(c, &req, &req.Kind) {
			return
		}
		a, err := s.merge(c.Request.Context(), req.Kind, req.SourceID, req.TargetID)
		tidyRespondAction(c, a, err)
	})
	g.POST("/rename", func(c *gin.Context) {
		var req struct {
			Kind string `json:"kind"`
			ID   int    `json:"id"`
			Name string `json:"name"`
		}
		if !tidyBind(c, &req, &req.Kind) {
			return
		}
		a, err := s.rename(c.Request.Context(), req.Kind, req.ID, req.Name)
		tidyRespondAction(c, a, err)
	})
	g.POST("/delete-many", func(c *gin.Context) {
		var req struct {
			Kind string `json:"kind"`
			IDs  []int  `json:"ids"`
		}
		if !tidyBind(c, &req, &req.Kind) {
			return
		}
		a, err := s.deleteMany(c.Request.Context(), req.Kind, req.IDs)
		tidyRespondAction(c, a, err)
	})
	g.POST("/delete", func(c *gin.Context) {
		var req struct {
			Kind string `json:"kind"`
			ID   int    `json:"id"`
		}
		if !tidyBind(c, &req, &req.Kind) {
			return
		}
		a, err := s.deleteEmpty(c.Request.Context(), req.Kind, req.ID)
		tidyRespondAction(c, a, err)
	})
	g.POST("/matching", func(c *gin.Context) {
		var req struct {
			Kind              string  `json:"kind"`
			IDs               []int   `json:"ids"`
			MatchingAlgorithm int     `json:"matching_algorithm"`
			Match             *string `json:"match"`
		}
		if !tidyBind(c, &req, &req.Kind) {
			return
		}
		a, err := s.setMatching(c.Request.Context(), req.Kind, req.IDs, req.MatchingAlgorithm, req.Match)
		tidyRespondAction(c, a, err)
	})
	g.POST("/grant-viewer", func(c *gin.Context) {
		var req struct {
			Kind string `json:"kind"`
			IDs  []int  `json:"ids"`
		}
		if !tidyBind(c, &req, &req.Kind) {
			return
		}
		a, err := s.grantViewer(c.Request.Context(), req.Kind, req.IDs)
		tidyRespondAction(c, a, err)
	})
	g.GET("/actions", func(c *gin.Context) {
		actions := []TidyAction{}
		if s.db != nil {
			limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
			if limit <= 0 || limit > 1000 {
				limit = 100
			}
			if err := s.db.Order("id desc").Limit(limit).Find(&actions).Error; err != nil {
				tidyRespond(c, nil, err)
				return
			}
		}
		c.JSON(http.StatusOK, actions)
	})
	g.POST("/actions/:id/undo", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil || id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad action id"})
			return
		}
		var req struct {
			Force bool `json:"force"`
		}
		_ = c.ShouldBindJSON(&req)
		a, err := s.undo(c.Request.Context(), uint(id), req.Force)
		tidyRespond(c, a, err)
	})
	g.DELETE("/aliases/:id", func(c *gin.Context) {
		if err := s.requireWrites(); err != nil {
			tidyRespond(c, nil, err)
			return
		}
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil || id <= 0 || s.db == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad alias id"})
			return
		}
		if err := s.db.Delete(&TidyAlias{}, id).Error; err != nil {
			tidyRespond(c, nil, err)
			return
		}
		s.invalidate()
		c.JSON(http.StatusOK, gin.H{"deleted": id})
	})
	g.POST("/split/start", func(c *gin.Context) {
		var req struct {
			CorrespondentID int  `json:"correspondent_id"`
			ExcludeSelf     bool `json:"exclude_self"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// The bucket is always left out of the choices: with it in, the model
		// can simply answer the bucket again (two runs did exactly that).
		run, err := s.startSplit(c.Request.Context(), req.CorrespondentID, true)
		tidyRespond(c, run, err)
	})
	g.GET("/split/runs", func(c *gin.Context) {
		runs := []TidySplitRun{}
		if s.db != nil {
			if err := s.db.Omit("results").Order("id desc").Limit(50).Find(&runs).Error; err != nil {
				tidyRespond(c, nil, err)
				return
			}
		}
		c.JSON(http.StatusOK, runs)
	})
	g.GET("/split/runs/:id", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil || id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad run id"})
			return
		}
		view, err := s.splitView(c.Request.Context(), uint(id))
		tidyRespond(c, view, err)
	})
	g.POST("/split/stop", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"stopping": s.stopSplit()})
	})
	g.POST("/fill/stop", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"stopping": s.stopSplit()})
	})
	g.POST("/fill/start", func(c *gin.Context) {
		var req struct {
			Limit int `json:"limit"`
		}
		_ = c.ShouldBindJSON(&req)
		run, err := s.startFill(c.Request.Context(), req.Limit)
		tidyRespond(c, run, err)
	})
	g.GET("/fill/runs", func(c *gin.Context) {
		runs := []TidyFillRun{}
		if s.db != nil {
			if err := s.db.Omit("results").Order("id desc").Limit(20).Find(&runs).Error; err != nil {
				tidyRespond(c, nil, err)
				return
			}
		}
		c.JSON(http.StatusOK, runs)
	})
	g.GET("/fill/runs/:id", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil || id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad run id"})
			return
		}
		view, err := s.fillView(c.Request.Context(), uint(id))
		tidyRespond(c, view, err)
	})
	g.GET("/fill/status", func(c *gin.Context) {
		st, err := s.fillStatus(c.Request.Context())
		tidyRespond(c, st, err)
	})
	// The ignore list is paperless-gpt's own and changes nothing in
	// paperless-ngx, so it works in read-only mode too.
	g.POST("/fill/ignore", func(c *gin.Context) {
		var req struct {
			Items []tidyFillIgnoreItem `json:"items"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		n, err := s.ignoreFill(req.Items)
		tidyRespond(c, gin.H{"ignored": n}, err)
	})
	g.POST("/fill/unignore", func(c *gin.Context) {
		var req struct {
			DocIDs []int `json:"doc_ids"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		n, err := s.unignoreFill(req.DocIDs)
		tidyRespond(c, gin.H{"unignored": n}, err)
	})
	g.POST("/fill/apply", func(c *gin.Context) {
		var req struct {
			RunID uint                `json:"run_id"`
			Items []tidyFillApplyItem `json:"items"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		a, err := s.applyFill(c.Request.Context(), req.RunID, req.Items)
		tidyRespondAction(c, a, err)
	})
	g.GET("/titles/status", func(c *gin.Context) {
		st, err := s.titleStatus(c.Request.Context())
		tidyRespond(c, st, err)
	})
	g.POST("/titles/start", func(c *gin.Context) {
		var req struct {
			Limit  int   `json:"limit"`
			DocIDs []int `json:"doc_ids"`
		}
		_ = c.ShouldBindJSON(&req)
		run, err := s.startTitles(c.Request.Context(), req.DocIDs, req.Limit)
		tidyRespond(c, run, err)
	})
	g.POST("/titles/stop", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"stopping": s.stopSplit()})
	})
	g.GET("/titles/runs", func(c *gin.Context) {
		runs := []TidyTitleRun{}
		if s.db != nil {
			if err := s.db.Omit("results").Order("id desc").Limit(20).Find(&runs).Error; err != nil {
				tidyRespond(c, nil, err)
				return
			}
		}
		c.JSON(http.StatusOK, runs)
	})
	g.GET("/titles/runs/:id", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil || id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad run id"})
			return
		}
		view, err := s.titleView(c.Request.Context(), uint(id))
		tidyRespond(c, view, err)
	})
	// Like Fill's, the title ignore list is paperless-gpt's own and works read-only.
	g.POST("/titles/ignore", func(c *gin.Context) {
		var req struct {
			Items []tidyFillIgnoreItem `json:"items"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		n, err := s.ignoreTitles(req.Items)
		tidyRespond(c, gin.H{"ignored": n}, err)
	})
	g.POST("/titles/unignore", func(c *gin.Context) {
		var req struct {
			DocIDs []int `json:"doc_ids"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		n, err := s.unignoreTitles(req.DocIDs)
		tidyRespond(c, gin.H{"unignored": n}, err)
	})
	// Date checks: read-only, with an ignore list of paperless-gpt's own.
	g.GET("/dates", func(c *gin.Context) {
		checks, err := s.dateChecks(c.Request.Context(), nil)
		tidyRespond(c, checks, err)
	})
	g.POST("/dates/ignore", func(c *gin.Context) {
		var req struct {
			Items []tidyDateIgnoreItem `json:"items"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		n, err := s.ignoreDates(req.Items)
		tidyRespond(c, gin.H{"ignored": n}, err)
	})
	g.POST("/dates/unignore", func(c *gin.Context) {
		var req struct {
			Items []tidyDateIgnoreItem `json:"items"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		n, err := s.unignoreDates(req.Items)
		tidyRespond(c, gin.H{"unignored": n}, err)
	})
	g.POST("/dates/created", func(c *gin.Context) {
		var req struct {
			Items []tidyDateSetItem `json:"items"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		a, err := s.setCreatedDates(c.Request.Context(), req.Items)
		tidyRespondAction(c, a, err)
	})
	// Documents paperless-gpt marked failed: why, and the ways out.
	g.GET("/failed", func(c *gin.Context) {
		view, err := s.failedView(c.Request.Context())
		tidyRespond(c, view, err)
	})
	g.POST("/failed/fix", func(c *gin.Context) {
		var req struct {
			DocID  int               `json:"doc_id"`
			Values []tidyFailedValue `json:"values"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		a, err := s.fixFailed(c.Request.Context(), req.DocID, req.Values)
		tidyRespondAction(c, a, err)
	})
	for path, retry := range map[string]bool{"/failed/clear": false, "/failed/retry": true} {
		g.POST(path, func(c *gin.Context) {
			var req struct {
				DocIDs []int `json:"doc_ids"`
			}
			if err := c.ShouldBindJSON(&req); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			a, err := s.clearFailed(c.Request.Context(), req.DocIDs, retry)
			tidyRespondAction(c, a, err)
		})
	}
	g.POST("/titles/apply", func(c *gin.Context) {
		var req struct {
			RunID uint                 `json:"run_id"`
			Items []tidyTitleApplyItem `json:"items"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		a, err := s.applyTitles(c.Request.Context(), req.RunID, req.Items)
		tidyRespondAction(c, a, err)
	})
	g.POST("/split/apply", func(c *gin.Context) {
		var req struct {
			RunID  uint                  `json:"run_id"`
			Groups []tidySplitApplyGroup `json:"groups"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		a, err := s.applySplit(c.Request.Context(), req.RunID, req.Groups)
		tidyRespondAction(c, a, err)
	})
	g.POST("/llm-groups", func(c *gin.Context) {
		var req struct {
			Kind string `json:"kind"`
		}
		if !tidyBind(c, &req, &req.Kind) {
			return
		}
		groups, err := s.llmGroups(c.Request.Context(), req.Kind)
		tidyRespond(c, groups, err)
	})
}

// tidyBind decodes a JSON body and checks its kind.
func tidyBind(c *gin.Context, req any, kind *string) bool {
	if err := c.ShouldBindJSON(req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return false
	}
	if !tidyValidKind(*kind) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown kind " + strconv.Quote(*kind)})
		return false
	}
	return true
}

func tidyRespond(c *gin.Context, body any, err error) {
	var refusal *tidyRefusal
	switch {
	case errors.As(err, &refusal):
		c.JSON(http.StatusConflict, refusal)
	case err != nil:
		log.Errorf("tidy: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusOK, body)
	}
}

// tidyRespondAction answers a write. An action that started but did not finish
// is returned along with the error, so the page can show what was logged.
func tidyRespondAction(c *gin.Context, a *TidyAction, err error) {
	var refusal *tidyRefusal
	if err != nil && a != nil && !errors.As(err, &refusal) {
		log.Errorf("tidy: %s %s %d: %v", a.Action, a.Kind, a.ObjectID, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "action": a})
		return
	}
	tidyRespond(c, a, err)
}
