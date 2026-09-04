package storage

import "crypto/sha1"

func sha1Sum(data []byte) []byte {
	h := sha1.Sum(data)
	return h[:]
}
