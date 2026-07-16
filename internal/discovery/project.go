package discovery

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

// ResolveProjectScope resolves the local authority used for discovery. Only
// the opaque ProjectRef is suitable for persistence.
func ResolveProjectScope(cwd string) (session.ProjectScope, error) {
	root, err := filepath.Abs(filepath.Clean(cwd))
	if err != nil {
		return session.ProjectScope{}, err
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		if os.IsNotExist(err) {
			return projectScope("unresolved", root), nil
		}
		return session.ProjectScope{}, err
	}
	root = canonical
	kind := "directory"
	if output, err := exec.Command("git", "-C", root, "rev-parse", "--show-toplevel").Output(); err == nil {
		if gitRoot, err := filepath.EvalSymlinks(strings.TrimSpace(string(output))); err == nil {
			root = gitRoot
			kind = "git_worktree"
		}
	}
	return projectScope(kind, root), nil
}

func projectScope(kind, root string) session.ProjectScope {
	sum := sha256.Sum256([]byte(kind + "\x00" + root))
	return session.ProjectScope{Ref: session.ProjectRef{Kind: kind, Key: "p_" + hex.EncodeToString(sum[:]), DisplayName: filepath.Base(root)}, CanonicalRoot: root}
}
