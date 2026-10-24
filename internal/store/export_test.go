package store

// SetFailAt makes s call failAt at every step of a change with more than
// one, and stop there if it returns an error.
func SetFailAt(s *Store, failAt func(step string) error) { s.failAt = failAt }
