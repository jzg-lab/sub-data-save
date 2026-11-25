package plugin

import (
	"time"

	"github.com/jzg-lab/sub-data-save/internal/archive"
)

var beijing = time.FixedZone("UTC+8", 8*60*60)

func latestNoon(now time.Time) time.Time {
	local := now.In(beijing)
	noon := time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, beijing)
	if local.Before(noon) {
		return noon.AddDate(0, 0, -1)
	}
	return noon
}

// Hold no capture or upload locks during compression. A config change replaces
// the timer; an old callback must not schedule another job for its former root.
func (s *Server) scheduleDailyLocked(root string, delay time.Duration) {
	store := s.store
	s.dailyTimer = time.AfterFunc(delay, func() {
		cutoff := latestNoon(time.Now())
		err := archive.BuildDailyZIP(root, cutoff, filesystemHasRoom)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.store != store {
			return
		}
		s.dailyFailed = err != nil
		next := time.Until(cutoff.AddDate(0, 0, 1))
		if err != nil {
			next = time.Minute
		}
		s.scheduleDailyLocked(root, next)
	})
}
