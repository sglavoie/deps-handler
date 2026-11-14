package cmd

import (
	"fmt"
	"slices"
	"sync"

	"github.com/sglavoie/deps-handler/pkg/report"
	"github.com/sglavoie/deps-handler/pkg/runner"
)

// runnerActivity keeps actual completions separate from config-ordered output.
// Prefer running reviews, then queued reviews, then repository reads/writes.
func runnerActivity(status *report.Status, total int, agent string) func(runner.Activity) {
	var mu sync.Mutex
	done := 0
	active := map[string]runner.Activity{}
	return func(a runner.Activity) {
		mu.Lock()
		defer mu.Unlock()
		key := fmt.Sprintf("%s#%d", a.Repo, a.PR)
		switch a.Phase {
		case "repository-finished":
			done++
			delete(active, key)
		case "review-finished":
			delete(active, key)
		default:
			active[key] = a
		}
		keys := make([]string, 0, len(active))
		for key := range active {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		message, priority := "finishing repositories", 0
		for _, key := range keys {
			work := active[key]
			switch {
			case work.Phase == "review-started" && priority < 3:
				message, priority = "reviewing "+key+" with "+agent, 3
			case work.Phase == "review-queued" && priority < 2:
				message, priority = "waiting for review slot for "+key, 2
			case priority < 1:
				message, priority = "processing "+work.Repo, 1
			}
		}
		status.Set(fmt.Sprintf("%s · %d/%d repos", message, done, total))
	}
}
