package main

import (
	"log/slog"
	"os"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := runRemote(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}
