package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sglavoie/deps-handler/pkg/config"
)

func filterRepos(cfg *config.Config, repos, owners []string, includeSkipped bool) ([]config.Repo, error) {
	selected, err := cfg.Filter(repos, owners, includeSkipped)
	var missing *config.SelectionError
	if !errors.As(err, &missing) {
		return selected, err
	}
	want := strings.ToLower(strings.TrimSpace(missing.Name))
	best, distance := "", 3 // Like Cobra, suggest only within distance 2.
	for _, repo := range cfg.Repos {
		candidate := repo.Name
		if missing.Kind == "owner" {
			candidate = repo.Owner
		} else if strings.Contains(want, "/") {
			candidate = repo.FullName()
		}
		if d := editDistance(want, strings.ToLower(candidate)); d < distance {
			best, distance = candidate, d
		}
	}
	if best != "" {
		// Disambiguate a bare name before suggesting a command-line value.
		if missing.Kind == "repo" && !strings.Contains(best, "/") {
			matches := 0
			fullName := ""
			for _, repo := range cfg.Repos {
				if strings.EqualFold(repo.Name, best) {
					matches++
					if fullName == "" {
						fullName = repo.FullName()
					}
				}
			}
			if matches > 1 {
				best = fullName
			}
		}
		err = fmt.Errorf("%w; did you mean %q?", err, best)
	}
	return selected, err
}

func editDistance(a, b string) int {
	x, y := []rune(a), []rune(b)
	row := make([]int, len(y)+1)
	for j := range row {
		row[j] = j
	}
	for i, r := range x {
		diagonal := row[0]
		row[0] = i + 1
		for j, s := range y {
			cost := 0
			if r != s {
				cost = 1
			}
			row[j+1], diagonal = min(row[j]+1, row[j+1]+1, diagonal+cost), row[j+1]
		}
	}
	return row[len(y)]
}
