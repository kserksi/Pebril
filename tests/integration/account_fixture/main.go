package main

import (
	"database/sql"
	"encoding/json"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(1)
	}
	var credentials struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.NewDecoder(os.Stdin).Decode(&credentials) != nil {
		os.Exit(1)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(credentials.Password), 12)
	if err != nil {
		os.Exit(1)
	}
	db, err := sql.Open("sqlite3", filepath.Join(os.Args[1], "pebril.db")+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		os.Exit(1)
	}
	defer db.Close()
	_, err = db.Exec("INSERT INTO users(username,password,quota) VALUES(?,?,1073741824) ON CONFLICT(username) DO NOTHING", credentials.Username, hash)
	if err != nil {
		os.Exit(1)
	}
}
