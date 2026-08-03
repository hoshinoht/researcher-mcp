package scholar

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"googlescholar-mcp-go/internal/config"
)

func newDocTestRequester(maxFetchBytes int64) *Requester {
	return NewRequester(config.Config{
		MaxRetries:    1,
		BackoffFactor: 1,
		UserAgents:    []string{"test-agent"},
		MaxFetchBytes: maxFetchBytes,
	})
}

func TestGetDocumentAcceptsPDFAndReportsContentType(t *testing.T) {
	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4 fake"))
	}))
	defer srv.Close()

	doc, err := newDocTestRequester(1024).GetDocument(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("GetDocument returned error: %v", err)
	}
	if !strings.Contains(gotAccept, "application/pdf") {
		t.Fatalf("Accept header %q does not mention application/pdf", gotAccept)
	}
	if doc.ContentType != "application/pdf" {
		t.Fatalf("ContentType = %q, want application/pdf", doc.ContentType)
	}
	if doc.Status != http.StatusOK {
		t.Fatalf("Status = %d, want 200", doc.Status)
	}
	if string(doc.Body) != "%PDF-1.4 fake" {
		t.Fatalf("Body = %q", doc.Body)
	}
}

func TestGetDocumentEnforcesSizeLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 2048))
	}))
	defer srv.Close()

	_, err := newDocTestRequester(1024).GetDocument(context.Background(), srv.URL)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
}

func TestGetDocumentReportsFinalURLAfterRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>done</html>"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	doc, err := newDocTestRequester(1024).GetDocument(context.Background(), srv.URL+"/start")
	if err != nil {
		t.Fatalf("GetDocument returned error: %v", err)
	}
	if doc.FinalURL != srv.URL+"/final" {
		t.Fatalf("FinalURL = %q, want %q", doc.FinalURL, srv.URL+"/final")
	}
}
