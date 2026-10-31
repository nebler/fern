package watch

// Decode returns how many events buf holds, as decode reads them.
func Decode(buf []byte) (int, error) {
	evs, err := decode(buf)
	return len(evs), err
}
