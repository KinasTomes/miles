package object

import (
	"github.com/KinasTomes/miles/internal/engine/write"
	"sync"
)

// Finish joins the admission heartbeat only after the managed session has
// joined its body/backend work. Repeated Close still retries reconciliation.
type admittedSession struct {
	*write.WriteSession
	finish func()
	once   sync.Once
}

func (s *admittedSession) Close() error {
	err := s.WriteSession.Close()
	s.once.Do(s.finish)
	return err
}
