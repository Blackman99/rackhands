// Package testbmc serves the Redfish mockups in testdata/mockups over
// httptest, recording every write, for unit tests. CI additionally runs the
// same mockups under the DMTF Redfish-Mockup-Server (test/integration.sh).
package testbmc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Write is one recorded POST/PATCH.
type Write struct {
	Method, Path, IfMatch string
	Body                  map[string]any
}

// Server is a running mock BMC.
type Server struct {
	*httptest.Server
	mu     sync.Mutex
	Writes []Write
	// RequireIfMatch makes PATCH without If-Match fail with 428, like XCC.
	RequireIfMatch bool
}

// Dir returns the mockup directory for a vendor ("dell", "hpe", …).
func Dir(vendor string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "mockups", vendor)
}

// Start serves a mockup until the test ends.
func Start(t testing.TB, vendor string) *Server {
	t.Helper()
	dir := Dir(vendor)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("mockup %s: %v", vendor, err)
	}
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/"), "/redfish/v1")
		file := filepath.Join(dir, filepath.FromSlash(p), "index.json")
		switch r.Method {
		case http.MethodGet:
			b, err := os.ReadFile(file)
			if err != nil {
				http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
				return
			}
			var o map[string]any
			_ = json.Unmarshal(b, &o)
			if et, ok := o["@odata.etag"].(string); ok {
				w.Header().Set("ETag", et)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		case http.MethodPost, http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			s.mu.Lock()
			s.Writes = append(s.Writes, Write{Method: r.Method, Path: r.URL.Path, IfMatch: r.Header.Get("If-Match"), Body: m})
			s.mu.Unlock()
			if r.Method == http.MethodPatch && s.RequireIfMatch && r.Header.Get("If-Match") == "" {
				w.WriteHeader(http.StatusPreconditionRequired)
				_, _ = w.Write([]byte(`{"error":{"@Message.ExtendedInfo":[{"Message":"If-Match header required"}]}}`))
				return
			}
			if strings.Contains(p, "SimpleUpdate") {
				w.Header().Set("Location", "/redfish/v1/TaskService/Tasks/JID_001")
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// Last returns the most recent write.
func (s *Server) Last() (Write, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Writes) == 0 {
		return Write{}, false
	}
	return s.Writes[len(s.Writes)-1], true
}

// Count returns the number of writes.
func (s *Server) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Writes)
}
