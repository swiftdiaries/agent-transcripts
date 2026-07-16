package session

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"sort"
	"strconv"
)

func ContentID(provider, sourceChecksum string) string {
	return "c_" + digest(provider, sourceChecksum)
}

// ContentIDForManifest derives a stable family content ID. Source order is
// intentionally canonicalized so filesystem enumeration cannot change it.
func ContentIDForManifest(provider string, manifest SourceManifest) string {
	entries := append([]SourceEntry(nil), manifest.Sources...)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Role != entries[j].Role {
			return entries[i].Role == "main"
		}
		return entries[i].AgentID < entries[j].AgentID
	})
	parts := []string{provider, manifest.Provider, manifest.SessionID}
	for _, entry := range entries {
		parts = append(parts, entry.Role, entry.AgentID, entry.Checksum, strconv.FormatInt(entry.Bytes, 10), entry.Name)
	}
	return "c_" + digest(parts...)
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
