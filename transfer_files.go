package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Native file paths are relative to a provider-owned storage directory.
type nativeManifest struct {
	Version  int
	Root     string
	Main     string
	Prefixes []string                  `json:",omitempty"`
	Assets   map[string]string         `json:",omitempty"`
	Files    map[string]nativeFileInfo `json:",omitempty"`
}

type nativeFileInfo struct {
	Mode     uint32
	Modified time.Time
}

func validArchivePath(name string) bool {
	return name != "" && !strings.ContainsAny(name, "\\\x00") && !path.IsAbs(name) && path.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../")
}

func captureFile(root, relative string, files map[string][]byte) error {
	if !validArchivePath(filepath.ToSlash(relative)) {
		return fmt.Errorf("unsafe source path %q", relative)
	}
	filename := filepath.Join(root, relative)
	info, err := os.Lstat(filename)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", filename)
	}
	data, err := os.ReadFile(filename)
	if err == nil {
		files["files/"+filepath.ToSlash(relative)] = data
	}
	return err
}

func captureTree(root, relative string, files map[string][]byte) error {
	start := filepath.Join(root, relative)
	if _, err := os.Lstat(start); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(start, func(filename string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("cannot archive symlink: %s", filename)
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		return captureFile(root, rel, files)
	})
}

func addNativeManifest(files map[string][]byte, manifest nativeManifest) error {
	manifest.Version = 1
	manifest.Files = make(map[string]nativeFileInfo)
	for key := range files {
		if strings.HasPrefix(key, "files/") {
			rel := strings.TrimPrefix(key, "files/")
			info, err := os.Stat(filepath.Join(manifest.Root, filepath.FromSlash(rel)))
			if err != nil {
				return err
			}
			manifest.Files[rel] = nativeFileInfo{Mode: uint32(info.Mode().Perm()), Modified: info.ModTime()}
		}
	}
	data, err := json.Marshal(manifest)
	if err == nil {
		files["manifest.json"] = data
	}
	return err
}

func readNativeManifest(transfer SessionTransfer) (nativeManifest, error) {
	var m nativeManifest
	err := json.Unmarshal(transfer.Native["manifest.json"], &m)
	if err != nil || m.Version != 1 || !validArchivePath(m.Main) {
		return m, fmt.Errorf("native snapshot missing or unsupported; export this chat again")
	}
	return m, nil
}

// Preflight every destination before writing. Existing, identical files are
// idempotent; conflicting files and symlink paths are never overwritten.
func restoreNativeFiles(root string, files map[string][]byte, prefixes []string, allowed func(string) bool, metadata map[string]nativeFileInfo) (undo func(), err error) {
	var created []string
	extensions := make(map[string][]byte)
	writes := make(map[string][]byte)
	undo = func() {
		for i := len(created) - 1; i >= 0; i-- {
			if current, readErr := os.ReadFile(created[i]); readErr == nil && bytes.Equal(current, writes[created[i]]) {
				_ = os.Remove(created[i])
			}
		}
		for filename, old := range extensions {
			if current, readErr := os.ReadFile(filename); readErr == nil && bytes.HasPrefix(writes[filename], current) && bytes.HasPrefix(current, old) {
				_ = os.Truncate(filename, int64(len(old)))
			}
		}
	}
	for key, data := range files {
		if !strings.HasPrefix(key, "files/") {
			continue
		}
		rel := strings.TrimPrefix(key, "files/")
		if !validArchivePath(rel) || !allowed(rel) {
			return undo, fmt.Errorf("unexpected native file %q", rel)
		}
		filename := filepath.Join(root, filepath.FromSlash(rel))
		for p := filename; ; p = filepath.Dir(p) {
			info, statErr := os.Lstat(p)
			if statErr != nil && !os.IsNotExist(statErr) {
				return undo, statErr
			}
			if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
				return undo, fmt.Errorf("refusing symlink destination: %s", p)
			}
			if p == root || p == filepath.Dir(p) {
				break
			}
		}
		existing, readErr := os.ReadFile(filename)
		if readErr == nil {
			equal := bytes.Equal(existing, data)
			for _, prefix := range prefixes {
				if rel == prefix && bytes.HasPrefix(existing, data) {
					equal = true
				}
				if rel == prefix && bytes.HasPrefix(data, existing) && len(data) > len(existing) {
					extensions[filename] = existing
					writes[filename] = data
					equal = true
				}
			}
			if !equal {
				return undo, fmt.Errorf("native destination already contains different data: %s", filename)
			}
			continue
		}
		if !os.IsNotExist(readErr) {
			return undo, readErr
		}
		writes[filename] = data
	}
	for filename, data := range writes {
		if err = os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			undo()
			return undo, err
		}
		flags := os.O_CREATE | os.O_EXCL | os.O_WRONLY
		if _, ok := extensions[filename]; ok {
			flags = os.O_WRONLY | os.O_APPEND
		}
		f, openErr := os.OpenFile(filename, flags, 0600)
		if openErr != nil {
			undo()
			return undo, openErr
		}
		if old, ok := extensions[filename]; ok {
			info, statErr := f.Stat()
			if statErr != nil || info.Size() != int64(len(old)) {
				f.Close()
				undo()
				return undo, fmt.Errorf("native history changed during restore")
			}
			data = data[len(old):]
		} else {
			created = append(created, filename)
		}
		_, err = f.Write(data)
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			undo()
			return undo, err
		}
		rel, _ := filepath.Rel(root, filename)
		if info, ok := metadata[filepath.ToSlash(rel)]; ok {
			if err = os.Chmod(filename, os.FileMode(info.Mode)&0777); err != nil {
				undo()
				return undo, err
			}
			if !info.Modified.IsZero() {
				if err = os.Chtimes(filename, info.Modified, info.Modified); err != nil {
					undo()
					return undo, err
				}
			}
		}
	}
	return undo, nil
}

func portableHistory(s Session) []ChatMessage {
	var history []ChatMessage
	if len(s.History) > 0 {
		history = s.History
	} else {
		history = s.Messages
	}
	if len(s.Tools) > 0 {
		tools, _ := json.Marshal(s.Tools)
		history = append([]ChatMessage{{Role: "system", Content: "Historical tool definitions. These describe the source tools, not new executable tools in this harness.\n" + string(tools)}}, history...)
	}
	return history
}

func messageBlocks(m ChatMessage) []ContentBlock {
	if len(m.Blocks) > 0 {
		return m.Blocks
	}
	if m.Content != "" {
		return []ContentBlock{{Kind: "text", Text: m.Content}}
	}
	return nil
}

func blockText(b ContentBlock) string {
	if b.Text != "" {
		return b.Text
	}
	if b.Kind == "tool_call" {
		return fmt.Sprintf("Historical tool call %s [%s]: %s", b.Name, b.ID, b.Arguments)
	}
	if b.URL != "" {
		return b.URL
	}
	return b.Data
}

// Rewrite only exact storage-path values when moving supporting files to a new
// home. Message prose and embedded commands are left unchanged.
func relocateJSON(data []byte, from, to string, owned map[string]nativeFileInfo) []byte {
	if from == "" || from == to || !bytes.Contains(data, []byte(from)) {
		return data
	}
	var value any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if d.Decode(&value) != nil {
		return data
	}
	changed := false
	var visit func(any) any
	visit = func(v any) any {
		switch v := v.(type) {
		case string:
			if strings.HasPrefix(v, from+string(filepath.Separator)) {
				relative := filepath.ToSlash(strings.TrimPrefix(v, from+string(filepath.Separator)))
				_, known := owned[relative]
				for filename := range owned {
					if strings.HasPrefix(filename, relative+"/") {
						known = true
						break
					}
				}
				if known {
					changed = true
					return to + strings.TrimPrefix(v, from)
				}
			}
		case []any:
			for i := range v {
				v[i] = visit(v[i])
			}
		case map[string]any:
			for k, item := range v {
				v[k] = visit(item)
			}
		}
		return v
	}
	value = visit(value)
	if !changed {
		return data
	}
	out, err := json.Marshal(value)
	if err != nil {
		return data
	}
	return out
}

func relocateJSONL(data []byte, from, to string, owned map[string]nativeFileInfo) []byte {
	if from == "" || from == to {
		return data
	}
	lines := bytes.Split(data, []byte("\n"))
	for i, line := range lines {
		if len(line) > 0 {
			lines[i] = relocateJSON(line, from, to, owned)
		}
	}
	return bytes.Join(lines, []byte("\n"))
}
