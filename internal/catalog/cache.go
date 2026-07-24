package catalog

import (
	"container/list"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

type Limits struct {
	MemoryEntries, DiskEntries int
	DiskBytes                  int64
}

var DefaultLimits = Limits{MemoryEntries: 32, DiskEntries: 256, DiskBytes: 512 << 20}

type Cache struct {
	mu      sync.Mutex
	dir     string
	limits  Limits
	order   *list.List
	entries map[string]*list.Element
}
type memoryEntry struct {
	key    string
	family session.SessionFamily
}
type diskEntry struct {
	FormatVersion int                   `json:"format_version"`
	RevisionKey   string                `json:"revision_key"`
	Family        session.SessionFamily `json:"family"`
}

var keyPattern = regexp.MustCompile(`^r_[a-f0-9]{64}$`)

func NewCache(dir string, limits Limits) *Cache {
	if limits.MemoryEntries == 0 {
		limits.MemoryEntries = DefaultLimits.MemoryEntries
	}
	if limits.DiskEntries == 0 {
		limits.DiskEntries = DefaultLimits.DiskEntries
	}
	if limits.DiskBytes == 0 {
		limits.DiskBytes = DefaultLimits.DiskBytes
	}
	return &Cache{dir: dir, limits: limits, order: list.New(), entries: map[string]*list.Element{}}
}
func clone(f session.SessionFamily) (session.SessionFamily, bool) {
	b, e := json.Marshal(f)
	if e != nil {
		return session.SessionFamily{}, false
	}
	var out session.SessionFamily
	if json.Unmarshal(b, &out) != nil {
		return session.SessionFamily{}, false
	}
	return out, true
}
func (c *Cache) Get(key string) (session.SessionFamily, bool) {
	if !keyPattern.MatchString(key) {
		return session.SessionFamily{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[key]; e != nil {
		c.order.MoveToFront(e)
		return clone(e.Value.(memoryEntry).family)
	}
	if c.dir == "" {
		return session.SessionFamily{}, false
	}
	b, e := os.ReadFile(filepath.Join(c.dir, key+".json"))
	if e != nil {
		return session.SessionFamily{}, false
	}
	var d diskEntry
	if json.Unmarshal(b, &d) != nil || d.FormatVersion != 1 || d.RevisionKey != key || session.ValidateFamily(d.Family) != nil {
		return session.SessionFamily{}, false
	}
	c.putMemory(key, d.Family)
	return clone(d.Family)
}
func (c *Cache) Put(key string, f session.SessionFamily) error {
	if !keyPattern.MatchString(key) || session.ValidateFamily(f) != nil {
		return nil
	}
	copy, ok := clone(f)
	if !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dir == "" {
		c.putMemory(key, copy)
		return nil
	}
	if err := os.MkdirAll(c.dir, 0700); err != nil {
		return err
	}
	_ = os.Chmod(c.dir, 0700)
	b, e := json.Marshal(diskEntry{1, key, copy})
	if e != nil {
		return e
	}
	tmp, e := os.CreateTemp(c.dir, ".entry-*")
	if e != nil {
		return e
	}
	name := tmp.Name()
	defer os.Remove(name)
	if e = tmp.Chmod(0600); e == nil {
		_, e = tmp.Write(b)
	}
	if e == nil {
		e = tmp.Sync()
	}
	if closeErr := tmp.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, filepath.Join(c.dir, key+".json")); e != nil {
		return e
	}
	c.putMemory(key, copy)
	return c.prune()
}
func (c *Cache) putMemory(key string, f session.SessionFamily) {
	if e := c.entries[key]; e != nil {
		e.Value = memoryEntry{key, f}
		c.order.MoveToFront(e)
		return
	}
	c.entries[key] = c.order.PushFront(memoryEntry{key, f})
	for c.order.Len() > c.limits.MemoryEntries {
		e := c.order.Back()
		delete(c.entries, e.Value.(memoryEntry).key)
		c.order.Remove(e)
	}
}
func (c *Cache) prune() error {
	files, e := filepath.Glob(filepath.Join(c.dir, "r_*.json"))
	if e != nil {
		return e
	}
	sort.Slice(files, func(i, j int) bool {
		a, _ := os.Stat(files[i])
		b, _ := os.Stat(files[j])
		return a.ModTime().Before(b.ModTime())
	})
	var bytes int64
	for _, f := range files {
		info, _ := os.Stat(f)
		if info != nil {
			bytes += info.Size()
		}
	}
	for len(files) > c.limits.DiskEntries || bytes > c.limits.DiskBytes {
		if len(files) <= 1 {
			break
		}
		info, _ := os.Stat(files[0])
		if info != nil {
			bytes -= info.Size()
		}
		_ = os.Remove(files[0])
		files = files[1:]
	}
	return nil
}
