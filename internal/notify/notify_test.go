package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oldsoska/uptrack/internal/monitor"
)

func downEvent() Event {
	opened := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	return Event{
		Kind:     KindDown,
		Monitor:  monitor.Monitor{ID: 7, Name: "Slotly", URL: "https://slotly-minx.onrender.com"},
		Incident: monitor.Incident{ID: 3, MonitorID: 7, OpenedAt: opened, Cause: "timed out after 10s"},
		At:       opened,
	}
}

func TestTelegramSendsMessage(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot123:abc/sendMessage" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tg := NewTelegram("123:abc", "-1001")
	tg.BaseURL = srv.URL
	if err := tg.Notify(context.Background(), downEvent()); err != nil {
		t.Fatal(err)
	}

	if got["chat_id"] != "-1001" {
		t.Errorf("chat_id = %v", got["chat_id"])
	}
	if text, _ := got["text"].(string); !strings.Contains(text, "Slotly is DOWN: timed out after 10s") {
		t.Errorf("text = %q", text)
	}
}

func TestTelegramErrorDoesNotLeakToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"ok":false,"description":"Unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	tg := NewTelegram("secret-token", "1")
	tg.BaseURL = srv.URL
	err := tg.Notify(context.Background(), downEvent())

	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("err = %v", err)
	}
}

func TestWebhookPostsJSON(t *testing.T) {
	var payload webhookPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content type = %s", ct)
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
	}))
	defer srv.Close()

	if err := NewWebhook(srv.URL).Notify(context.Background(), downEvent()); err != nil {
		t.Fatal(err)
	}
	if payload.Event != KindDown || payload.MonitorID != 7 || payload.IncidentID != 3 || payload.Cause == "" {
		t.Errorf("payload = %+v", payload)
	}
}

type flaky struct {
	failures int32
	calls    atomic.Int32
}

func (f *flaky) Notify(context.Context, Event) error {
	if f.calls.Add(1) <= f.failures {
		return errors.New("temporary failure")
	}
	return nil
}

func TestRetryRecoversFromTransientFailures(t *testing.T) {
	f := &flaky{failures: 2}
	err := Retry{Next: f, Attempts: 3, Base: time.Millisecond}.Notify(context.Background(), downEvent())

	if err != nil || f.calls.Load() != 3 {
		t.Fatalf("err = %v, calls = %d", err, f.calls.Load())
	}
}

func TestRetryGivesUp(t *testing.T) {
	f := &flaky{failures: 10}
	err := Retry{Next: f, Attempts: 3, Base: time.Millisecond}.Notify(context.Background(), downEvent())

	if err == nil || f.calls.Load() != 3 {
		t.Fatalf("err = %v, calls = %d", err, f.calls.Load())
	}
}

func TestMultiDeliversToAllChannelsDespiteOneFailing(t *testing.T) {
	failing, healthy := &flaky{failures: 1}, &flaky{}
	err := Multi{failing, healthy}.Notify(context.Background(), downEvent())

	if err == nil || healthy.calls.Load() != 1 {
		t.Fatalf("err = %v, healthy calls = %d", err, healthy.calls.Load())
	}
}

func TestRecoverySummaryIncludesOutageDuration(t *testing.T) {
	e := downEvent()
	resolved := e.Incident.OpenedAt.Add(7*time.Minute + 30*time.Second)
	e.Kind, e.Incident.ResolvedAt, e.At = KindRecovered, &resolved, resolved

	if got, want := e.Summary(), "Slotly is back UP after 7m30s"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestNetworkErrorKeepsCauseButNotToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // connection refused

	tg := NewTelegram("secret-token", "1")
	tg.BaseURL = srv.URL
	err := tg.Notify(context.Background(), downEvent())

	if err == nil || strings.Contains(err.Error(), "secret-token") || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v", err)
	}
}
