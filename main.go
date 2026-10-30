package main

import (
	_ "embed"
	"os"

	"github.com/sglavoie/deps-handler/cmd"
)

//go:embed config.example.yaml
var exampleConfig []byte

func main() {
	cmd.ExampleConfig = exampleConfig
	os.Exit(cmd.Execute())
}
