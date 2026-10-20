package monitor

import (
	"testing"
	"time"
)

func TestStateApply(t *testing.T) {
	ok := Result{OK: true}
	fail := Result{OK: false, Error: "connection refused"}

	tests := []struct {
		name       string
		start      State
		results    []Result
		threshold  int
		want       []Transition
		wantStatus Status
	}{
		{
			name:       "first success makes an unknown monitor UP without a recovery alert",
			start:      State{Status: StatusUnknown},
			results:    []Result{ok},
			threshold:  2,
			want:       []Transition{NoChange},
			wantStatus: StatusUp,
		},
		{
			name:       "one failure below the threshold changes nothing",
			start:      State{Status: StatusUp},
			results:    []Result{fail, ok},
			threshold:  2,
			want:       []Transition{NoChange, NoChange},
			wantStatus: StatusUp,
		},
		{
			name:       "reaching the threshold goes down exactly once",
			start:      State{Status: StatusUp},
			results:    []Result{fail, fail, fail, fail},
			threshold:  2,
			want:       []Transition{NoChange, WentDown, NoChange, NoChange},
			wantStatus: StatusDown,
		},
		{
			name:       "first success after an outage recovers",
			start:      State{Status: StatusDown, ConsecutiveFailures: 5},
			results:    []Result{ok, ok},
			threshold:  2,
			want:       []Transition{Recovered, NoChange},
			wantStatus: StatusUp,
		},
		{
			name:       "a never-reached service goes down from unknown",
			start:      State{Status: StatusUnknown},
			results:    []Result{fail},
			threshold:  1,
			want:       []Transition{WentDown},
			wantStatus: StatusDown,
		},
		{
			name:       "a success in between resets the failure count",
			start:      State{Status: StatusUp},
			results:    []Result{fail, ok, fail},
			threshold:  2,
			want:       []Transition{NoChange, NoChange, NoChange},
			wantStatus: StatusUp,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.start
			for i, r := range tt.results {
				if got := s.Apply(r, tt.threshold); got != tt.want[i] {
					t.Fatalf("result %d: got %v, want %v", i, got, tt.want[i])
				}
			}
			if s.Status != tt.wantStatus {
				t.Errorf("status = %s, want %s", s.Status, tt.wantStatus)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	valid := Monitor{
		Name: "Slotly", URL: "https://slotly-minx.onrender.com/actuator/health", Method: "GET",
		Interval: time.Minute, Timeout: 10 * time.Second, ExpectedStatusMin: 200, ExpectedStatusMax: 399,
		FailureThreshold: 2,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid monitor rejected: %v", err)
	}

	broken := []struct {
		name   string
		mutate func(*Monitor)
	}{
		{"empty name", func(m *Monitor) { m.Name = "  " }},
		{"ftp url", func(m *Monitor) { m.URL = "ftp://example.com" }},
		{"relative url", func(m *Monitor) { m.URL = "/health" }},
		{"POST method", func(m *Monitor) { m.Method = "POST" }},
		{"interval too short", func(m *Monitor) { m.Interval = 5 * time.Second }},
		{"timeout not shorter than interval", func(m *Monitor) { m.Interval = 30 * time.Second; m.Timeout = 30 * time.Second }},
		{"inverted status range", func(m *Monitor) { m.ExpectedStatusMin = 400; m.ExpectedStatusMax = 200 }},
		{"zero threshold", func(m *Monitor) { m.FailureThreshold = 0 }},
	}
	for _, tt := range broken {
		t.Run(tt.name, func(t *testing.T) {
			m := valid
			tt.mutate(&m)
			if m.Validate() == nil {
				t.Error("expected a validation error")
			}
		})
	}
}
