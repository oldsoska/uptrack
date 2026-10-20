package monitor

// Transition is what a check changed about a monitor's status.
type Transition int

const (
	NoChange Transition = iota
	WentDown
	Recovered
)

func (t Transition) String() string {
	switch t {
	case WentDown:
		return "went_down"
	case Recovered:
		return "recovered"
	default:
		return "no_change"
	}
}

// State is the part of a monitor that checks update.
type State struct {
	Status              Status
	ConsecutiveFailures int
}

// Apply folds one result into the state.
//
// A monitor goes DOWN only after `threshold` consecutive failures, and comes back UP
// on the first success. UNKNOWN (never checked) becomes UP silently: there was no
// outage to recover from, so it is not reported as a recovery.
func (s *State) Apply(r Result, threshold int) Transition {
	if r.OK {
		s.ConsecutiveFailures = 0
		previous := s.Status
		s.Status = StatusUp
		if previous == StatusDown {
			return Recovered
		}
		return NoChange
	}

	s.ConsecutiveFailures++
	if s.ConsecutiveFailures >= threshold && s.Status != StatusDown {
		s.Status = StatusDown
		return WentDown
	}
	return NoChange
}
