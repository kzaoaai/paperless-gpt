package main

// A way to try the Tidy page with no paperless-ngx, no container and no
// model: the fake paperless-ngx from tidy_test.go, the real Tidy routes and
// the built web app on one local port. The LLM is a stub with fixed answers,
// so no prompt is sent anywhere. It only runs when TIDY_DEMO_ADDR is set:
//
//	(cd web-app && npm run build)
//	TIDY_DEMO_ADDR=127.0.0.1:8091 go test -run TestTidyDemoServer -timeout 0 .
//
// Open http://127.0.0.1:8091/tidy. It stops on GET /demo/stop, or after
// TIDY_DEMO_MINUTES (default 20).

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestTidyDemoServer(t *testing.T) {
	addr := os.Getenv("TIDY_DEMO_ADDR")
	if addr == "" {
		t.Skip("set TIDY_DEMO_ADDR to serve the Tidy page against a fake paperless-ngx")
	}
	if _, err := os.Stat("web-app/dist/index.html"); err != nil {
		t.Fatal("build the web app first: (cd web-app && npm run build)")
	}
	f := newFakePaperless(t)
	f.users = []tidyUser{{ID: 3, Username: "mcp-reader"}}
	for id, name := range map[int]string{10: "Taxes", 11: "Payment", 12: "Purchase", 13: "Solar", 14: "Healthcare"} {
		f.addObject("tags", id, name, map[string]any{"permissions": perm(3)})
	}
	f.addObject("document_types", 20, "Instructions / Manual", map[string]any{"permissions": perm(3)})
	f.addObject("document_types", 21, "Invoice / Receipt", map[string]any{"permissions": perm(3)})
	f.addObject("correspondents", 30, "Example Appliances", map[string]any{"permissions": perm(3)})
	f.addObject("correspondents", 31, "Example Bank", map[string]any{"permissions": perm(3)})
	titles := []string{
		"Dishwasher User Manual", "Wire transfer receipt", "Solar inverter user guide", "Clinic visit summary",
		"Dryer Operation Manual", "Warehouse club receipt", "Dehumidifier Instruction Manual", "Annual statement",
		"Monitor arm Installation Manual", "Parent handbook", "Bonus letter", "Water softener Service Manual",
	}
	for i, title := range titles {
		id := 101 + i
		d := tidyDoc{ID: id, Added: "2026-0" + strconv.Itoa(1+i%9) + "-0" + strconv.Itoa(1+i%9) + "T00:00:00Z", Correspondent: ip(30 + i%2)}
		switch {
		case i == 7:
			d.Tags, d.DocumentType = []int{10}, ip(21) // complete
		case i == 9 || i == 10:
			// no tags and no type
		default:
			d.DocumentType = ip(20 + i%2)
		}
		f.addDoc(d)
		f.titles[id] = title
	}
	s := newTestTidy(t, f)
	// Fixed answers: nothing for a manual, as the tag prompt allows.
	s.fillSuggest = func(ctx context.Context, id int, wantTags, wantType bool) (string, []string, string, error) {
		title := f.titles[id]
		var tags []string
		docType := ""
		if wantTags && !strings.Contains(title, "Manual") {
			tags = []string{"Payment", "purchase", "Made Up"}
			if strings.Contains(title, "Solar") {
				tags = []string{"Solar"}
			}
		}
		if wantType {
			docType = "Invoice / Receipt"
		}
		return title, tags, docType, nil
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	stop := make(chan struct{})
	r.GET("/demo/stop", func(c *gin.Context) {
		c.String(http.StatusOK, "stopping\n")
		select {
		case <-stop:
		default:
			close(stop)
		}
	})
	r.GET("/api/paperless-url", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"url": "http://paperless.invalid"}) })
	s.routes(r.Group("/api/tidy"))
	r.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.JSON(http.StatusNotFound, gin.H{"error": "not part of the demo"})
			return
		}
		file := filepath.Join("web-app/dist", filepath.Clean("/"+c.Request.URL.Path))
		if st, err := os.Stat(file); err == nil && !st.IsDir() {
			c.File(file)
			return
		}
		c.File("web-app/dist/index.html")
	})
	srv := &http.Server{Addr: addr, Handler: r}
	go func() { _ = srv.ListenAndServe() }()
	minutes, _ := strconv.Atoi(os.Getenv("TIDY_DEMO_MINUTES"))
	if minutes <= 0 {
		minutes = 20
	}
	t.Logf("Tidy demo on http://%s/tidy for %d minutes", addr, minutes)
	select {
	case <-stop:
	case <-time.After(time.Duration(minutes) * time.Minute):
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
