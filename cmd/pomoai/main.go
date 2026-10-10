package main

import (
	"os"

	pomoai "github.com/PomoAi-ai/pomoai-cli"
)

func main() { os.Exit(pomoai.Main(os.Args[1:])) }
