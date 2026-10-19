package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type duplicateResult struct {
	ID       string
	Path     string
	Resume   string
	Mode     string
	Warnings []string
}

// duplicateSession uses the source provider's snapshot for native cloning or translation.
func duplicateSession(s Session, target SessionProvider, home string) (duplicateResult, error) {
	source, err := findProvider(s.ProviderName)
	if err != nil {
		return duplicateResult{}, err
	}
	transfer, err := source.CaptureSession(s)
	if err != nil {
		return duplicateResult{}, err
	}
	return target.ImportSession(transfer, home, true)
}

// Remove incomplete native files on failure and never overwrite a session.
func writeImportedJSONL(path string, write func(*json.Encoder) error) (err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if err = write(json.NewEncoder(f)); err != nil {
		return err
	}
	return f.Close()
}
