// Package testbmc serves the Redfish mockups in testdata/mockups over
// httptest, recording every write, for unit tests. CI additionally runs the
// same mockups under the DMTF Redfish-Mockup-Server (test/integration.sh).
//
// Resources with a _steps directory change over time: the n-th GET of that
// path returns _steps/n.json until they run out, then index.json (the final
// state, which is all the Redfish-Mockup-Server ever serves). Task monitors
// (…/TaskMonitors/…) answer 202 Accepted, as DSP0266 requires while a task runs.
package testbmc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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
	// TaskLocation is the Location header SimpleUpdate answers with (202).
	// Empty means 204 with no task, like iLO 5. Defaults per vendor.
	TaskLocation string
	// Outages makes the next N GETs under /TaskService/ (or of UpdateService)
	// fail with 503, like a BMC that restarts to apply its own firmware.
	Outages int
	gets    map[string]int
}

// SimpleUpdate answers per vendor: iDRAC points at its job's Task, XCC at a
// DSP0266 task monitor, iLO 5 returns no task at all.
var taskLocation = map[string]string{
	"dell":   "/redfish/v1/TaskService/Tasks/JID_001",
	"lenovo": "/redfish/v1/TaskService/TaskMonitors/1",
	"hpe":    "",
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
	s := &Server{gets: map[string]int{}, TaskLocation: "/redfish/v1/TaskService/Tasks/JID_001"}
	if loc, ok := taskLocation[vendor]; ok {
		s.TaskLocation = loc
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/"), "/redfish/v1")
		file := filepath.Join(dir, filepath.FromSlash(p), "index.json")
		switch r.Method {
		case http.MethodGet:
			s.mu.Lock()
			n := s.gets[p]
			s.gets[p]++
			down := s.Outages > 0 && (strings.HasPrefix(p, "/TaskService/") || p == "/UpdateService")
			if down {
				s.Outages--
			}
			s.mu.Unlock()
			if down {
				http.Error(w, `{"error":{"message":"service temporarily unavailable"}}`, http.StatusServiceUnavailable)
				return
			}
			if steps, _ := filepath.Glob(filepath.Join(filepath.Dir(file), "_steps", "*.json")); n < len(steps) {
				sort.Strings(steps)
				file = steps[n]
			}
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
			if strings.Contains(p, "/TaskMonitors/") {
				w.WriteHeader(http.StatusAccepted)
			}
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
				s.mu.Lock()
				loc := s.TaskLocation
				s.mu.Unlock()
				if loc == "" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Header().Set("Location", loc)
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

// Gets returns how many times path (e.g. "/TaskService/Tasks/1") was read.
func (s *Server) Gets(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets[path]
}

// SetOutages arms n 503 answers for task / UpdateService reads.
func (s *Server) SetOutages(n int) {
	s.mu.Lock()
	s.Outages = n
	s.mu.Unlock()
}

// Count returns the number of writes.
func (s *Server) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Writes)
}
