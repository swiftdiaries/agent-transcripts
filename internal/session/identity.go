package session

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
)

func ContentID(provider, sourceChecksum string) string {
	return "c_" + digest(provider, sourceChecksum)
}

func PackageID(contentID string, destination Directory) string {
	return "s_" + digest(contentID, destination.Kind, destination.Slug)
}

func digest(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		writeDelimited(h, part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeDelimited(h hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(value))
}
