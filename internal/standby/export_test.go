package standby

import "time"

// Age moves every box's last result back by d, as if d had passed with nothing
// reaching the box.
func (s *Set) Age(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.members {
		if !m.at.IsZero() {
			m.at = m.at.Add(-d)
		}
	}
}
