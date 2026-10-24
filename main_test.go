package main

import (
	"testing"

	"github.com/sglavoie/deps-merger/internal/config"
)

// The example init writes must be a config deps-merger accepts.
func TestExampleConfigParses(t *testing.T) {
	if _, err := config.Parse(exampleConfig); err != nil {
		t.Fatal(err)
	}
}
