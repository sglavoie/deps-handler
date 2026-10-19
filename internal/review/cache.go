package review

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sglavoie/deps-merger/internal/policy"
)

// CacheTTL is how long a verdict is reused. Dependabot reopens an update
// as a new PR or new versions, which get a fresh review anyway; this only
// bounds how long a judgement made on old release notes stands.
const CacheTTL = 14 * 24 * time.Hour

// Cache keeps verdicts between runs in a JSON file so a scheduled run does
// not pay for the same review again. It is safe for concurrent use, and
// each write starts from the file as it is then, so it keeps what another
// process stored or forgot meanwhile.
type Cache struct {
	path    string
	now     func() time.Time
	mu      sync.Mutex
	entries map[string]cacheEntry
	// recovered says what happened to an unreadable cache file.
	recovered string
}

type cacheEntry struct {
	Verdict policy.Verdict `json:"verdict"`
	Agent   string         `json:"agent"`
	// HeadOID is the commit reviewed. A rejection holds for that commit
	// only: once Dependabot rebases the PR, the base it now sits on may
	// have dealt with what the agent objected to.
	HeadOID    string    `json:"head_oid,omitempty"`
	ReviewedAt time.Time `json:"reviewed_at"`
}

// Entry is one cached verdict.
type Entry struct {
	Key   string
	Agent string
	// HeadOID is the commit reviewed; a rejection holds for it only.
	HeadOID    string
	ReviewedAt time.Time
	// Expires is when the verdict stops being reused.
	Expires time.Time
	Verdict policy.Verdict
}

// OpenCache reads the cache at path; a missing or empty file is an empty
// cache. A file that cannot be read as one is moved aside, and
// TakeRecovered says so.
func OpenCache(path string) (*Cache, error) {
	c := &Cache{path: path, now: time.Now}
	entries, err := c.readEntries()
	if err != nil {
		return nil, err
	}
	c.entries = entries
	return c, nil
}

// TakeRecovered describes the unreadable cache file this cache last moved
// aside and started over from, on opening or on a write since, or is empty
// when there was none. Each is described once, so a caller can ask again
// after every write and warn about each.
func (c *Cache) TakeRecovered() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	msg := c.recovered
	c.recovered = ""
	return msg
}

// readEntries reads the cache file. One that is not valid JSON, as when a
// crash cut a write short on a filesystem that does not order it, is
// renamed to path.corrupt-TIMESTAMP so that the verdicts in it are not
// lost and the cache works again.
func (c *Cache) readEntries() (map[string]cacheEntry, error) {
	entries := map[string]cacheEntry{}
	data, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) || err == nil && len(bytes.TrimSpace(data)) == 0 {
		return entries, nil
	}
	if err != nil {
		return nil, err
	}
	var stored map[string]cacheEntry
	if perr := json.Unmarshal(data, &stored); perr != nil {
		aside := c.path + ".corrupt-" + c.now().UTC().Format("20060102T150405Z")
		if err := os.Rename(c.path, aside); errors.Is(err, os.ErrNotExist) {
			return entries, nil // another process moved it aside first
		} else if err != nil {
			return nil, fmt.Errorf("%s: %w (and could not move it aside: %v)", c.path, perr, err)
		}
		c.recovered = fmt.Sprintf("verdict cache %s was unreadable (%v); moved it to %s and started with no cached verdicts", c.path, perr, aside)
		return entries, nil
	}
	for k, e := range stored {
		// Keys written before the repository was lower-cased in them.
		k = normKey(k)
		if old, ok := entries[k]; !ok || e.ReviewedAt.After(old.ReviewedAt) {
			entries[k] = e
		}
	}
	return entries, nil
}

// normKey lower-cases the repository ("owner/name") at the start of key,
// as Request.Key does.
func normKey(key string) string {
	i := strings.IndexAny(key, "# ")
	if i < 0 {
		i = len(key)
	}
	return strings.ToLower(key[:i]) + key[i:]
}

// Get returns the verdict stored under key for the PR at head, unless it
// has expired or is a rejection of another head.
func (c *Cache) Get(key, head string) *policy.Verdict {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[normKey(key)]
	if !ok || c.expired(e) || !e.Verdict.Approved && e.HeadOID != "" && e.HeadOID != head {
		return nil
	}
	return &e.Verdict
}

// Entries returns the verdicts that have not expired, by key.
func (c *Cache) Entries() []Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Entry
	for k, e := range c.entries {
		if !c.expired(e) {
			out = append(out, Entry{Key: k, Agent: e.Agent, HeadOID: e.HeadOID, ReviewedAt: e.ReviewedAt, Expires: e.ReviewedAt.Add(CacheTTL), Verdict: e.Verdict})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Put stores the verdict on the PR at head and writes the cache.
func (c *Cache) Put(key, head, agent string, v policy.Verdict) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.update(func(entries map[string]cacheEntry) {
		entries[normKey(key)] = cacheEntry{Verdict: v, Agent: agent, HeadOID: head, ReviewedAt: c.now()}
	})
}

// Forget drops the verdicts of a repository ("owner/name"), of one PR
// ("owner/name#7") or under one full key, ignoring case, and returns how
// many it dropped.
func (c *Cache) Forget(target string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	err := c.update(func(entries map[string]cacheEntry) {
		for k := range entries {
			if Matches(k, target) {
				delete(entries, k)
				n++
			}
		}
	})
	return n, err
}

// Matches reports whether key ("owner/name#7 lib@2.0.0") is target or
// starts with it up to a "#" or a space, ignoring case.
func Matches(key, target string) bool {
	if len(key) < len(target) || !strings.EqualFold(key[:len(target)], target) {
		return false
	}
	return len(key) == len(target) || key[len(target)] == '#' || key[len(target)] == ' '
}

func (c *Cache) expired(e cacheEntry) bool { return c.now().Sub(e.ReviewedAt) > CacheTTL }

// update applies change to the entries on disk, drops expired ones and
// writes them back, holding a lock file so that another process's write
// cannot land between the read and the write and be lost. c.mu must be
// held.
func (c *Cache) update(change func(map[string]cacheEntry)) error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	unlock, err := waitLock(c.path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	entries, err := c.readEntries()
	if err != nil {
		return err
	}
	change(entries)
	for k, e := range entries {
		if c.expired(e) {
			delete(entries, k)
		}
	}
	c.entries = entries
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".verdicts-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Have the data on disk before the rename can be, so a crash cannot
	// leave an empty or partial cache in place of the old one.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.path)
}
