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
	// Titles as an old import left them, for the title re-check.
	for i, title := range []string{"Scan_20230412", "41ead9a1-381b-4cbb-95e7-2f080ff63b4f", "Invoice_4471.pdf", "Paychecks", "ID Card Front"} {
		id := 113 + i
		f.addDoc(tidyDoc{ID: id, Added: "2023-04-0" + strconv.Itoa(1+i) + "T00:00:00Z", Correspondent: ip(31), DocumentType: ip(21), Tags: []int{11},
			OriginalFileName: title + ".pdf"})
		f.titles[id] = title
	}
	// Dates: two pieces split out of document 113 a year and a half later (one
	// already given its source's date), a notice dated after it arrived, and a
	// piece of document 90, which is gone: its date is estimated from 101's.
	f.addDoc(tidyDoc{ID: 143, Title: "Lease addendum", OriginalFileName: "90_edit_1.pdf", Created: "2025-12-31", Added: "2026-09-13T04:00:00+03:00",
		Correspondent: ip(31), DocumentType: ip(21), Tags: []int{11}})
	f.addDoc(tidyDoc{ID: 140, Title: "Paystub - 2017-06-07", OriginalFileName: "113_1-1.pdf", Created: "2017-06-07", Added: "2024-11-17T04:20:26+02:00",
		Correspondent: ip(31), DocumentType: ip(21), Tags: []int{11}})
	f.addDoc(tidyDoc{ID: 141, Title: "Paystub - 2017-06-21", OriginalFileName: "113_2-2.pdf", Created: "2017-06-21", Added: "2023-04-01T00:00:00Z",
		Correspondent: ip(31), DocumentType: ip(21), Tags: []int{11}})
	f.addDoc(tidyDoc{ID: 142, Title: "Registration renewal notice", Created: "2024-07-08", Added: "2024-04-18T10:00:00+03:00",
		Correspondent: ip(31), DocumentType: ip(21), Tags: []int{11}})
	// Failed documents: one with a value paperless-ngx refused, one paperless-gpt
	// gave up on, and one marked with no record of why.
	f.addObject("tags", 15, "paperless-gpt-auto", map[string]any{"permissions": perm(3)})
	f.addObject("tags", 16, "paperless-gpt-failed", map[string]any{"permissions": perm(3)})
	f.addObject("tags", 17, "paperless-gpt-ocr-auto", map[string]any{"permissions": perm(3)})
	f.addObject("custom_fields", 40, "Details", map[string]any{"data_type": "string"})
	for id, title := range map[int]string{150: "Certificate of insurance 2026-2027", 151: "Scan_0042", 152: "Parking permit"} {
		f.addDoc(tidyDoc{ID: id, Title: title, Created: "2026-11-04", Added: "2026-11-17T19:52:28Z", Correspondent: ip(31), DocumentType: ip(21), Tags: []int{11, 16}})
	}
	s := newTestTidy(t, f)
	s.configRefs = func() tidyConfigRefs {
		return tidyConfigRefs{TagNames: map[string]string{"paperless-gpt-auto": "AUTO_TAG", "paperless-gpt-failed": "FAIL_TAG"}, CustomFieldIDs: map[int]string{},
			AutoTag: "paperless-gpt-auto", OCRTag: "paperless-gpt-ocr-auto", FailTag: "paperless-gpt-failed"}
	}
	saveProcessingFailures(s.db, []ProcessingFailure{
		{DocumentID: 150, Kind: failureRejectedField, Field: "custom_fields", FieldID: 40,
			Value:  "Master policy for the homeowners association: general liability, property and directors and officers cover, 2026-11-04 to 2027-11-04",
			Reason: "Ensure this value has at most 128 characters (it has 132)."},
		{DocumentID: 151, Kind: failureGaveUp, Stage: "paperless-gpt-ocr-auto", Reason: "OCR failed 3 times"},
	})
	s.titleSuggest = func(ctx context.Context, id int) (string, string, error) {
		return f.titles[id], "Example Bank - Statement - " + strconv.Itoa(4000+id) + " - 2023-04", nil
	}
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
