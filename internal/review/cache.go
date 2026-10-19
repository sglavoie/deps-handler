package review

import (
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
}

type cacheEntry struct {
	Verdict    policy.Verdict `json:"verdict"`
	Agent      string         `json:"agent"`
	ReviewedAt time.Time      `json:"reviewed_at"`
}

// Entry is one cached verdict.
type Entry struct {
	Key        string
	Agent      string
	ReviewedAt time.Time
	Verdict    policy.Verdict
}

// OpenCache reads the cache at path; a missing file is an empty cache.
func OpenCache(path string) (*Cache, error) {
	entries, err := readEntries(path)
	if err != nil {
		return nil, err
	}
	return &Cache{path: path, now: time.Now, entries: entries}, nil
}

func readEntries(path string) (map[string]cacheEntry, error) {
	entries := map[string]cacheEntry{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return entries, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("%s: %w (delete it to start over)", path, err)
	}
	return entries, nil
}

// Get returns the verdict stored under key, unless it has expired.
func (c *Cache) Get(key string) *policy.Verdict {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || c.expired(e) {
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
			out = append(out, Entry{Key: k, Agent: e.Agent, ReviewedAt: e.ReviewedAt, Verdict: e.Verdict})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Put stores a verdict and writes the cache.
func (c *Cache) Put(key, agent string, v policy.Verdict) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.update(func(entries map[string]cacheEntry) {
		entries[key] = cacheEntry{Verdict: v, Agent: agent, ReviewedAt: c.now()}
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
			if matches(k, target) {
				delete(entries, k)
				n++
			}
		}
	})
	return n, err
}

// matches reports whether key ("owner/name#7 lib@2.0.0") is target or
// starts with it up to a "#" or a space.
func matches(key, target string) bool {
	if len(key) < len(target) || !strings.EqualFold(key[:len(target)], target) {
		return false
	}
	return len(key) == len(target) || key[len(target)] == '#' || key[len(target)] == ' '
}

func (c *Cache) expired(e cacheEntry) bool { return c.now().Sub(e.ReviewedAt) > CacheTTL }

// update applies change to the entries on disk, drops expired ones and
// writes them back. c.mu must be held.
func (c *Cache) update(change func(map[string]cacheEntry)) error {
	entries, err := readEntries(c.path)
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
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
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
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.path)
}
