package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
)

// Read portable records and opaque native bundles without extracting ZIP paths.
func readArchive(filename string) ([]SessionTransfer, error) {
	z, err := zip.OpenReader(filename)
	if err != nil {
		return nil, err
	}
	defer z.Close()
	const maxBytes = 2 << 30
	var total uint64
	entries := make(map[string][]byte)
	var order []string
	for _, file := range z.File {
		if file.FileInfo().IsDir() {
			continue
		}
		if !validArchivePath(file.Name) || !file.Mode().IsRegular() {
			return nil, fmt.Errorf("unsafe archive entry %q", file.Name)
		}
		if _, ok := entries[file.Name]; ok {
			return nil, fmt.Errorf("duplicate archive entry %q", file.Name)
		}
		if file.UncompressedSize64 > maxBytes-total {
			return nil, fmt.Errorf("archive exceeds 2 GiB import limit")
		}
		r, err := file.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(r, int64(maxBytes-total)+1))
		closeErr := r.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if uint64(len(data)) > maxBytes-total {
			return nil, fmt.Errorf("archive exceeds import limit")
		}
		total += uint64(len(data))
		entries[file.Name] = data
		if path.Base(file.Name) == "conversation.json" && !strings.Contains(path.Dir(file.Name), "/") {
			order = append(order, file.Name)
		}
	}
	if len(order) == 0 || len(order) > 10000 {
		return nil, fmt.Errorf("archive must contain 1 to 10000 conversations")
	}
	var transfers []SessionTransfer
	for _, name := range order {
		var s Session
		if err := json.Unmarshal(entries[name], &s); err != nil {
			return nil, err
		}
		if s.ID == "" || s.ProviderName == "" {
			return nil, fmt.Errorf("invalid conversation in %s", name)
		}
		prefix := strings.TrimSuffix(name, "conversation.json")
		transfer := SessionTransfer{Session: s, Native: make(map[string][]byte)}
		for key, data := range entries {
			if strings.HasPrefix(key, prefix+"native/") {
				transfer.Native[strings.TrimPrefix(key, prefix+"native/")] = data
			}
		}
		if data, ok := entries[prefix+"warnings.json"]; ok {
			if err := json.Unmarshal(data, &transfer.Warnings); err != nil {
				return nil, err
			}
		}
		transfers = append(transfers, transfer)
	}
	return transfers, nil
}

func importArchive(filename, target, home string, output io.Writer) error {
	provider, err := findProvider(target)
	if err != nil {
		return err
	}
	transfers, err := readArchive(filename)
	if err != nil {
		return err
	}
	for i, transfer := range transfers {
		result, err := provider.ImportSession(transfer, home, false)
		if err != nil {
			return fmt.Errorf("import stopped after %d of %d chats: %w", i, len(transfers), err)
		}
		fmt.Fprintf(output, "Imported %s [%s]\nProject: %s\nResume: %s\n", singleLine(transfer.Session.DisplayName()), result.Mode, singleLine(transfer.Session.Project), result.Resume)
		for _, warning := range result.Warnings {
			fmt.Fprintf(output, "Notice: %s\n", singleLine(warning))
		}
		fmt.Fprintln(output)
	}
	fmt.Fprintf(output, "Imported %d chat(s) into %s.\n", len(transfers), provider.Name())
	return nil
}
