package auth

import (
	"sync"
	"testing"
)

func TestReview13LastActiveAdmin(t *testing.T) {
	s, ck := newService(t)
	_, id := enrolledAdmin(t, s, ck)
	id.Reverified = true
	if _, e := s.CreateUser(id, "second", "", "admin"); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); <-start; errs <- s.SetRole(id, 1, "user") }()
	go func() { defer wg.Done(); <-start; errs <- s.SetStatus(id, 2, "disabled") }()
	close(start)
	wg.Wait()
	close(errs)
	successes := 0
	for e := range errs {
		if e == nil {
			successes++
		} else if !is(e, ErrLastAdmin) {
			t.Fatal(e)
		}
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin' AND status='active'`).Scan(&n)
	if n != 1 || successes != 1 {
		t.Fatalf("active admins=%d successful removals=%d", n, successes)
	}
}
