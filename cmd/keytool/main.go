// Command keytool generates an API key and appends its SHA-256 digest to a
// key table. The plaintext key is printed once, to stdout, and never stored.
//
//	go run ./cmd/keytool -keys-file keys.json -id alice -role customer -customer-id 7001
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"

	"orderguard/internal/auth"
)

func main() {
	file := flag.String("keys-file", "keys.json", "key table to create or append to")
	id := flag.String("id", "", "key identifier (appears in logs)")
	role := flag.String("role", string(auth.RoleCustomer), "customer or support")
	customerID := flag.String("customer-id", "", "customer ID (customer role only)")
	flag.Parse()

	if err := run(*file, auth.KeyEntry{ID: *id, Role: auth.Role(*role), CustomerID: *customerID}); err != nil {
		fmt.Fprintln(os.Stderr, "keytool:", err)
		os.Exit(1)
	}
}

func run(path string, entry auth.KeyEntry) error {
	var entries []auth.KeyEntry
	f, err := os.Open(path)
	switch {
	case err == nil:
		entries, err = auth.ParseKeys(f)
		f.Close()
		if err != nil {
			return err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}

	key, err := auth.GenerateKey()
	if err != nil {
		return err
	}
	entry.KeySHA256 = auth.HashKey(key)
	entries = append(entries, entry)
	if _, err := auth.NewAuthenticator(entries); err != nil { // validate before writing
		return err
	}

	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Println(key)
	return nil
}
