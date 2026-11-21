package catalog

// Waiters reports how many Sync or Await calls wait on connector's latest
// run: the rerun queued behind the running one, else the running one. A
// test releases a blocked run once its callers have joined.
//
// The lister's gate cannot stand in: a caller joins the rerun inside Sync
// without touching the lister, so only the Syncer can tell that it has.
func Waiters(s *Syncer, connector string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.flights[job{connector: connector}]

	if !ok {
		return 0
	}

	if f.next != nil {
		return f.next.waiters
	}

	return f.cur.waiters
}
