//go:build !unix

package taskenvdocker

import "errors"

func prepareRoot(string) (string, [32]byte, error) {
	return "", [32]byte{}, errors.New("background run Docker state requires Unix")
}
func diskAvailable(string) (int64, error) {
	return 0, errors.New("background run Docker state requires Unix")
}
