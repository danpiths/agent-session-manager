package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
)

// exportSessions writes a private archive in the application's launch directory.
// Native records preserve tool calls and other data absent from the text preview.
func exportSessions(directory, label string, sessions []Session) (path string, err error) {
	if len(sessions) == 0 {
		return "", fmt.Errorf("no chats selected")
	}
	f, err := os.CreateTemp(directory, "chats-"+safeName(label)+"-*.zip")
	if err != nil {
		return "", err
	}
	path = f.Name()
	archivePath := path
	defer func() {
		if err != nil {
			_ = os.Remove(archivePath)
		}
	}()
	z := zip.NewWriter(f)
	for i, session := range sessions {
		prefix := fmt.Sprintf("%03d-%s/", i+1, safeName(session.DisplayName()))
		if err = exportSession(z, prefix, session); err != nil {
			_ = z.Close()
			_ = f.Close()
			return "", fmt.Errorf("exporting %s: %w", session.DisplayName(), err)
		}
	}
	err = z.Close()
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return path, err
}

func exportSession(z *zip.Writer, prefix string, s Session) error {
	provider, err := findProvider(s.ProviderName)
	if err != nil {
		return err
	}
	transfer, err := provider.CaptureSession(s)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(transfer.Session, "", "  ")
	if err != nil {
		return err
	}
	if err = zipBytes(z, prefix+"conversation.json", data); err != nil {
		return err
	}
	var transcript strings.Builder
	fmt.Fprintf(&transcript, "# %s\n\nProvider: %s\nProject: %s\nSession: %s\n", s.DisplayName(), s.ProviderName, s.Project, s.ID)
	for _, message := range portableHistory(transfer.Session) {
		fmt.Fprintf(&transcript, "\n## %s\n\n", message.Role)
		for _, block := range messageBlocks(message) {
			writeTranscriptBlock(&transcript, block)
		}
	}
	if err = zipBytes(z, prefix+"conversation.md", []byte(transcript.String())); err != nil {
		return err
	}
	for name, data := range transfer.Native {
		if !validArchivePath(name) {
			return fmt.Errorf("unsafe native file %q", name)
		}
		if err = zipBytes(z, prefix+"native/"+name, data); err != nil {
			return err
		}
	}
	warnings, _ := json.Marshal(transfer.Warnings)
	return zipBytes(z, prefix+"warnings.json", warnings)
}

func writeTranscriptBlock(out *strings.Builder, block ContentBlock) {
	switch block.Kind {
	case "tool_call":
		fmt.Fprintf(out, "Tool call %s [%s]\n\n%s\n", block.Name, block.ID, block.Arguments)
	case "tool_result":
		fmt.Fprintf(out, "Tool result [%s], error=%t\n\n%s\n", block.ID, block.IsError, block.Text)
	case "image", "file", "audio":
		fmt.Fprintf(out, "[%s attachment, %s, preserved in conversation.json]\n", block.Kind, block.MediaType)
		if block.URL != "" {
			fmt.Fprintf(out, "%s\n", block.URL)
		}
	default:
		fmt.Fprintf(out, "%s\n", blockText(block))
	}
	for _, child := range block.Children {
		writeTranscriptBlock(out, child)
	}
}

func zipBytes(z *zip.Writer, name string, data []byte) error {
	w, err := z.Create(name)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func zipFile(z *zip.Writer, name, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetModTime(time.Now())
	header.SetMode(0600)
	w, err := z.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

func safeName(s string) string {
	var name strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			name.WriteRune(r)
		} else {
			name.WriteByte('-')
		}
		if name.Len() >= 64 {
			break
		}
	}
	result := strings.Trim(name.String(), "-")
	if result == "" {
		return "conversation"
	}
	return result
}
