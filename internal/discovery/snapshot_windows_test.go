//go:build windows

package discovery

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestSnapshotFamilyFailureRemovesPrivateFilesOnWindows(t *testing.T) {
	snapshotRoot := t.TempDir()
	t.Setenv("TMP", snapshotRoot)
	t.Setenv("TEMP", snapshotRoot)

	family, mutateBetweenPasses := mutatingFamily(t)
	snapshotTestHook = mutateBetweenPasses
	t.Cleanup(func() { snapshotTestHook = nil })
	if _, err := SnapshotFamily(context.Background(), family); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("err=%v", err)
	}
	entries, err := os.ReadDir(snapshotRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("private snapshots remain after failed capture: %v", entries)
	}
}
