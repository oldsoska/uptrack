package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/oldsoska/uptrack/internal/metrics"
	"github.com/oldsoska/uptrack/internal/monitor"
	"github.com/oldsoska/uptrack/internal/store"
)

const token = "test-admin-token-0123456789abcdef"

var testStore *store.Store

func TestMain(m *testing.M) {
	ctx := context.Background()
	pg, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("uptrack"), postgres.WithUsername("uptrack"), postgres.WithPassword("uptrack"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(60*time.Second)))
	if err != nil {
		panic(err)
	}
	url, _ := pg.ConnectionString(ctx, "sslmode=disable")
	if testStore, err = store.Open(ctx, url); err != nil {
		panic(err)
	}
	if err := testStore.Migrate(ctx); err != nil {
		panic(err)
	}
	code := m.Run()
	testStore.Close()
	_ = pg.Terminate(ctx)
	os.Exit(code)
}

// fakeJobs records what the API told the scheduler.
type fakeJobs struct {
	mu       sync.Mutex
	upserted []monitor.Monitor
	removed  []int64
}

func (f *fakeJobs) Upsert(m monitor.Monitor) {
	f.mu.Lock()
	f.upserted = append(f.upserted, m)
	f.mu.Unlock()
}
func (f *fakeJobs) Remove(id int64) { f.mu.Lock(); f.removed = append(f.removed, id); f.mu.Unlock() }

type client struct {
	t   *testing.T
	srv *httptest.Server
}

func newClient(t *testing.T) (*client, *fakeJobs) {
	jobs := &fakeJobs{}
	api := New(testStore, jobs, token, metrics.New().Registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv}, jobs
}

func (c *client) do(method, path, auth string, body any) (*http.Response, []byte) {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, reader)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func (c *client) create(name string) monitorResponse {
	c.t.Helper()
	resp, body := c.do("POST", "/api/v1/monitors", token, map[string]any{"name": name, "url": "https://example.com/" + name})
	if resp.StatusCode != http.StatusCreated {
		c.t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var m monitorResponse
	_ = json.Unmarshal(body, &m)
	return m
}

func TestAdminAPIRequiresToken(t *testing.T) {
	c, _ := newClient(t)
	for _, auth := range []string{"", "wrong-token", token + "x"} {
		resp, body := c.do("GET", "/api/v1/monitors", auth, nil)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Content-Type") != "application/problem+json" {
			t.Errorf("auth %q: %d %s", auth, resp.StatusCode, body)
		}
	}
}

func TestCreateAppliesDefaultsAndSchedules(t *testing.T) {
	c, jobs := newClient(t)

	m := c.create("defaults")

	if m.Method != "GET" || m.IntervalSeconds != 60 || m.TimeoutSeconds != 10 || m.FailureThreshold != 2 ||
		m.ExpectedStatusMin != 200 || m.ExpectedStatusMax != 399 || m.Status != monitor.StatusUnknown {
		t.Errorf("defaults not applied: %+v", m)
	}
	if len(jobs.upserted) != 1 || jobs.upserted[0].ID != m.ID {
		t.Errorf("scheduler not told about the new monitor: %+v", jobs.upserted)
	}
}

func TestCreateRejectsInvalidInput(t *testing.T) {
	c, jobs := newClient(t)
	cases := []struct {
		name string
		body any
		want int
	}{
		{"unknown field", map[string]any{"name": "x", "url": "https://x.io", "intervall": 60}, http.StatusBadRequest},
		{"wrong type", map[string]any{"name": "x", "url": "https://x.io", "intervalSeconds": "60"}, http.StatusBadRequest},
		{"not http", map[string]any{"name": "x", "url": "file:///etc/passwd"}, http.StatusUnprocessableEntity},
		{"too frequent", map[string]any{"name": "x", "url": "https://x.io", "intervalSeconds": 5}, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := c.do("POST", "/api/v1/monitors", token, tc.body)
			if resp.StatusCode != tc.want {
				t.Errorf("got %d %s, want %d", resp.StatusCode, body, tc.want)
			}
		})
	}
	if len(jobs.upserted) != 0 {
		t.Errorf("invalid monitors reached the scheduler: %+v", jobs.upserted)
	}
}

func TestGetUpdateAndDelete(t *testing.T) {
	c, jobs := newClient(t)
	m := c.create("lifecycle")
	path := "/api/v1/monitors/" + strconv.FormatInt(m.ID, 10)

	resp, body := c.do("GET", path, token, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"last7d"`) {
		t.Fatalf("get: %d %s", resp.StatusCode, body)
	}

	resp, body = c.do("PUT", path, token, map[string]any{"name": "renamed", "url": m.URL, "paused": true})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"paused":true`) {
		t.Fatalf("update: %d %s", resp.StatusCode, body)
	}
	if last := jobs.upserted[len(jobs.upserted)-1]; !last.Paused || last.Name != "renamed" {
		t.Errorf("scheduler got stale monitor: %+v", last)
	}

	if resp, _ = c.do("DELETE", path, token, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if len(jobs.removed) != 1 || jobs.removed[0] != m.ID {
		t.Errorf("scheduler not told about the deletion: %v", jobs.removed)
	}
	if resp, _ = c.do("GET", path, token, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("deleted monitor still found: %d", resp.StatusCode)
	}
}

func TestBadIDAndLimit(t *testing.T) {
	c, _ := newClient(t)
	if resp, _ := c.do("GET", "/api/v1/monitors/abc", token, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("non-numeric id: %d", resp.StatusCode)
	}
	if resp, _ := c.do("GET", "/api/v1/incidents?limit=100000", token, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("huge limit: %d", resp.StatusCode)
	}
}

func TestStatusPageIsPublicAndHidesURLs(t *testing.T) {
	c, _ := newClient(t)
	m := c.create("public-page")
	if _, err := testStore.RecordCheck(context.Background(),
		monitor.Result{MonitorID: m.ID, CheckedAt: time.Now(), OK: true, StatusCode: 200},
		monitor.State{Status: monitor.StatusUp}, monitor.NoChange); err != nil {
		t.Fatal(err)
	}

	resp, body := c.do("GET", "/status", "", nil)

	page := string(body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "public-page") || !strings.Contains(page, "100.00%") {
		t.Fatalf("status page: %d\n%s", resp.StatusCode, page)
	}
	if strings.Contains(page, m.URL) {
		t.Error("public page leaks the monitored URL")
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Error("missing Content-Security-Policy")
	}
}

func TestStatusPageEscapesNames(t *testing.T) {
	c, _ := newClient(t)
	c.create("<script>alert(1)</script>")

	_, body := c.do("GET", "/status", "", nil)

	if strings.Contains(string(body), "<script>alert(1)") {
		t.Error("monitor name rendered unescaped")
	}
}

func TestHealthAndMetricsArePublic(t *testing.T) {
	c, _ := newClient(t)
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		if resp, _ := c.do("GET", path, "", nil); resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
}
