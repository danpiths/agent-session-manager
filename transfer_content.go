package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func decodeJSONLines(data []byte) ([]map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var records []map[string]any
	for {
		var record map[string]any
		err := d.Decode(&record)
		if err == io.EOF {
			return records, nil
		}
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
}

func storageRoot(filename, section string) (string, error) {
	for dir := filepath.Dir(filename); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if filepath.Base(dir) == section {
			return filepath.Dir(dir), nil
		}
	}
	return "", fmt.Errorf("session is outside %s storage", section)
}

func portableImage(source, mediaType, data string) ContentBlock {
	b := ContentBlock{Kind: "image", URL: source, MediaType: mediaType, Data: data}
	if strings.HasPrefix(source, "data:") {
		header, encoded, ok := strings.Cut(strings.TrimPrefix(source, "data:"), ",")
		if ok && strings.HasSuffix(header, ";base64") {
			b.MediaType = strings.TrimSuffix(header, ";base64")
			b.Data = encoded
			b.URL = ""
		}
	}
	if b.MediaType == "" && b.URL != "" {
		b.MediaType = mime.TypeByExtension(filepath.Ext(strings.TrimPrefix(b.URL, "file://")))
	}
	return b
}

func mediaURL(b ContentBlock) string {
	if b.Data != "" {
		return "data:" + b.MediaType + ";base64," + b.Data
	}
	return b.URL
}

// Include local media bytes so a transfer survives removal of the source home.
func captureMedia(transfer *SessionTransfer, manifest *nativeManifest) error {
	callNames := make(map[string]string)
	for i := range transfer.Session.History {
		for j := range transfer.Session.History[i].Blocks {
			block := &transfer.Session.History[i].Blocks[j]
			if block.Kind == "tool_call" {
				callNames[block.ID] = block.Name
			}
			if block.Kind == "tool_result" && block.Name == "" {
				block.Name = callNames[block.ID]
			}
		}
	}
	manifest.Assets = make(map[string]string)
	var capture func(*ContentBlock) error
	capture = func(b *ContentBlock) error {
		for i := range b.Children {
			if err := capture(&b.Children[i]); err != nil {
				return err
			}
		}
		if b.Kind != "image" && b.Kind != "file" && b.Kind != "audio" {
			return nil
		}
		filename := b.URL
		if strings.HasPrefix(filename, "file://") {
			u, err := url.Parse(filename)
			if err != nil {
				return err
			}
			filename = u.Path
		}
		if !filepath.IsAbs(filename) {
			return nil
		}
		var data []byte
		var err error
		if b.Data != "" {
			data, err = base64.StdEncoding.DecodeString(b.Data)
		} else {
			info, statErr := os.Lstat(filename)
			if statErr != nil {
				return fmt.Errorf("reading referenced media %s: %w", filename, statErr)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("referenced media is not a regular file: %s", filename)
			}
			data, err = os.ReadFile(filename)
		}
		if err != nil {
			return err
		}
		hash := sha256.Sum256(data)
		key := fmt.Sprintf("assets/%x%s", hash, filepath.Ext(filename))
		transfer.Native[key] = data
		manifest.Assets[b.URL] = key
		b.Data = base64.StdEncoding.EncodeToString(data)
		if b.MediaType == "" {
			b.MediaType = mime.TypeByExtension(filepath.Ext(filename))
		}
		return nil
	}
	for i := range transfer.Session.History {
		for j := range transfer.Session.History[i].Blocks {
			if err := capture(&transfer.Session.History[i].Blocks[j]); err != nil {
				return err
			}
		}
	}
	return nil
}

func uniqueWarnings(warnings []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, warning := range warnings {
		if warning != "" && !seen[warning] {
			seen[warning] = true
			result = append(result, warning)
		}
	}
	return result
}

func validatePortable(s Session) error {
	if !filepath.IsAbs(s.Project) {
		return fmt.Errorf("chat has no absolute project directory")
	}
	if len(portableHistory(s)) == 0 {
		return fmt.Errorf("chat has no transferable history")
	}
	for _, message := range portableHistory(s) {
		switch message.Role {
		case "user", "assistant", "system", "developer", "tool":
		default:
			return fmt.Errorf("unsupported portable message role %q", message.Role)
		}
	}
	return nil
}

func prepareNativeMedia(transfer SessionTransfer, manifest nativeManifest, root, id string) (map[string][]byte, map[string]string, error) {
	files := make(map[string][]byte)
	replacements := make(map[string]string)
	for original, key := range manifest.Assets {
		if !validArchivePath(key) || !strings.HasPrefix(key, "assets/") {
			return nil, nil, fmt.Errorf("invalid media entry %q", key)
		}
		data, ok := transfer.Native[key]
		if !ok {
			return nil, nil, fmt.Errorf("native media is missing: %s", key)
		}
		filename := strings.TrimPrefix(original, "file://")
		if existing, err := os.ReadFile(filename); err == nil && bytes.Equal(existing, data) {
			continue
		}
		rel := "transfer-assets/" + id + "/" + filepath.Base(key)
		destination := filepath.Join(root, filepath.FromSlash(rel))
		files["files/"+rel] = data
		replacements[original] = destination
		for _, kind := range []string{"file", "audio"} {
			replacements["Imported "+kind+" attachment: "+original] = "Imported " + kind + " attachment: " + destination
		}
		if strings.HasPrefix(original, manifest.Root+string(filepath.Separator)) {
			replacements[root+strings.TrimPrefix(original, manifest.Root)] = destination
		}
	}
	return files, replacements, nil
}

// Retain the portable meaning of translations that a native transcript can only
// express as text. The semantic prefix hash prevents stale metadata from reviving
// messages removed by compaction, editing, or rollback in the destination app.
type portableMetadata struct {
	History     []ChatMessage
	Tools       []ToolDefinition
	NativeCount int
	NativeHash  string
}

func historyHash(history []ChatMessage) string {
	copy := append([]ChatMessage(nil), history...)
	normalizeText := func(text string) string {
		for _, kind := range []string{"file", "audio"} {
			prefix := "Imported " + kind + " attachment: "
			if strings.HasPrefix(text, prefix) {
				if index := strings.Index(text, "/transfer-assets/"); index >= 0 {
					tail := text[index+len("/transfer-assets/"):]
					if split := strings.IndexByte(tail, '/'); split >= 0 {
						return prefix + "<stored-asset>/" + tail[split+1:]
					}
				}
			}
		}
		return text
	}
	var normalizeBlock func(ContentBlock) ContentBlock
	normalizeBlock = func(b ContentBlock) ContentBlock {
		if b.Data != "" {
			b.URL = ""
		}
		b.Text = normalizeText(b.Text)
		b.Children = append([]ContentBlock(nil), b.Children...)
		for i := range b.Children {
			b.Children[i] = normalizeBlock(b.Children[i])
		}
		return b
	}
	for i := range copy {
		copy[i].Timestamp = time.Time{}
		copy[i].Content = normalizeText(copy[i].Content)
		copy[i].Blocks = append([]ContentBlock(nil), copy[i].Blocks...)
		for j := range copy[i].Blocks {
			copy[i].Blocks[j] = normalizeBlock(copy[i].Blocks[j])
		}
	}
	data, _ := json.Marshal(copy)
	hash := sha256.Sum256(data)
	return fmt.Sprintf("%x", hash)
}

func capturePortableMetadata(root, id string, transfer *SessionTransfer) error {
	if err := captureTree(root, filepath.Join(".session-manager", id), transfer.Native); err != nil {
		return err
	}
	rel := filepath.Join(".session-manager", id+".json")
	data, err := os.ReadFile(filepath.Join(root, rel))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var meta portableMetadata
	if err = json.Unmarshal(data, &meta); err != nil {
		return err
	}
	if meta.NativeCount <= len(transfer.Session.History) && meta.NativeCount >= 0 && historyHash(transfer.Session.History[:meta.NativeCount]) == meta.NativeHash {
		transfer.Session.History = append(append([]ChatMessage(nil), meta.History...), transfer.Session.History[meta.NativeCount:]...)
		transfer.Session.Tools = meta.Tools
	} else {
		transfer.Warnings = append(transfer.Warnings, "The native history was edited or compacted; stale portable metadata was not reapplied.")
	}
	return captureFile(root, rel, transfer.Native)
}

func preservePortableMetadata(provider SessionProvider, root string, result duplicateResult, source Session, original SessionTransfer) error {
	canonical := SessionTransfer{Session: source, Native: make(map[string][]byte)}
	if len(canonical.Session.History) == 0 {
		canonical.Session.History = append([]ChatMessage(nil), source.Messages...)
	}
	if err := captureMedia(&canonical, &nativeManifest{}); err != nil {
		return err
	}
	source = canonical.Session
	native := source
	native.ID = result.ID
	native.FilePath = result.Path
	native.ProviderName = provider.Name()
	captured, err := provider.CaptureSession(native)
	if err != nil {
		return err
	}
	history := source.History
	if len(history) == 0 {
		history = source.Messages
	}
	meta := portableMetadata{History: history, Tools: source.Tools, NativeCount: len(captured.Session.History), NativeHash: historyHash(captured.Session.History)}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	rel := ".session-manager/" + result.ID + ".json"
	files := map[string][]byte{"files/" + rel: data}
	prefix := ".session-manager/" + result.ID + "/original/"
	record, marshalErr := json.Marshal(original.Session)
	if marshalErr != nil {
		return marshalErr
	}
	files["files/"+prefix+"conversation.json"] = record
	for name, contents := range original.Native {
		if !validArchivePath(name) {
			return fmt.Errorf("unsafe original native file %q", name)
		}
		files["files/"+prefix+"native/"+name] = contents
	}
	_, err = restoreNativeFiles(root, files, nil, func(name string) bool { return name == rel || strings.HasPrefix(name, prefix) }, nil)
	return err
}

func materializePortableFiles(root, id string, s *Session) error {
	if len(s.History) == 0 {
		s.History = append([]ChatMessage(nil), s.Messages...)
	}
	var visit func(*ContentBlock) error
	visit = func(b *ContentBlock) error {
		for i := range b.Children {
			if err := visit(&b.Children[i]); err != nil {
				return err
			}
		}
		if (b.Kind != "file" && b.Kind != "audio") || b.Data == "" {
			return nil
		}
		data, err := base64.StdEncoding.DecodeString(b.Data)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(data)
		exts, _ := mime.ExtensionsByType(b.MediaType)
		ext := ""
		if len(exts) > 0 {
			ext = exts[0]
		}
		rel := fmt.Sprintf("transfer-assets/%s/%x%s", id, hash, ext)
		if _, err = restoreNativeFiles(root, map[string][]byte{"files/" + rel: data}, nil, func(string) bool { return true }, nil); err != nil {
			return err
		}
		b.URL = filepath.Join(root, filepath.FromSlash(rel))
		return nil
	}
	for i := range s.History {
		for j := range s.History[i].Blocks {
			if err := visit(&s.History[i].Blocks[j]); err != nil {
				return err
			}
		}
	}
	return nil
}

func replaceJSONLValues(data []byte, replacements map[string]string) []byte {
	if len(replacements) == 0 {
		return data
	}
	lines := bytes.Split(data, []byte("\n"))
	for i, line := range lines {
		if len(line) > 0 {
			lines[i] = replaceJSONValues(line, replacements)
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

func rewriteJSONLines(data []byte, change func(map[string]any)) ([]byte, error) {
	records, err := decodeJSONLines(data)
	if err != nil {
		return nil, err
	}
	var result bytes.Buffer
	encoder := json.NewEncoder(&result)
	for _, record := range records {
		change(record)
		if err := encoder.Encode(record); err != nil {
			return nil, err
		}
	}
	return result.Bytes(), nil
}

func replaceJSONValues(data []byte, replacements map[string]string) []byte {
	if len(replacements) == 0 {
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
	visit = func(item any) any {
		switch v := item.(type) {
		case string:
			if replacement, ok := replacements[v]; ok {
				changed = true
				return replacement
			}
		case map[string]any:
			for key, item := range v {
				v[key] = visit(item)
			}
		case []any:
			for i, item := range v {
				v[i] = visit(item)
			}
		}
		return item
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
