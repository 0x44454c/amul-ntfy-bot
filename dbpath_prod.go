//go:build prod

package main

import (
	"log"
	"os"
	"path/filepath"
)

var databasePath string

func init() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal("Failed to get home directory:", err)
	}
	databasePath = filepath.Join(home, "ntfy-bot", "data", "amul.db")
}
