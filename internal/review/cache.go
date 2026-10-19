package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sglavoie/deps-merger/internal/policy"
)

// CacheTTL is how long a verdict is reused. Dependabot reopens an update
// as a new PR or new versions, which get a fresh review anyway; this only
// bounds how long a judgement made on old release notes stands.
const CacheTTL = 14 * 24 * time.Hour

// Cache keeps verdicts between runs in a JSON file so a scheduled run does
// not pay for the same review again. It is safe for concurrent use.
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

// OpenCache reads the cache at path; a missing file is an empty cache.
func OpenCache(path string) (*Cache, error) {
	c := &Cache{path: path, now: time.Now, entries: map[string]cacheEntry{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &c.entries); err != nil {
		return nil, fmt.Errorf("%s: %w (delete it to start over)", path, err)
	}
	return c, nil
}

// Get returns the verdict stored under key, unless it has expired.
func (c *Cache) Get(key string) *policy.Verdict {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || c.now().Sub(e.ReviewedAt) > CacheTTL {
		return nil
	}
	return &e.Verdict
}

// Put stores a verdict and writes the cache, dropping expired entries.
func (c *Cache) Put(key, agent string, v policy.Verdict) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.entries[key] = cacheEntry{Verdict: v, Agent: agent, ReviewedAt: now}
	for k, e := range c.entries {
		if now.Sub(e.ReviewedAt) > CacheTTL {
			delete(c.entries, k)
		}
	}
	data, err := json.MarshalIndent(c.entries, "", "  ")
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
