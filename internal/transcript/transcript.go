// Package transcript reads agent session logs and normalises the user and
// assistant entries for display: Claude Code's (~/.claude/projects/*/<id>.jsonl)
// and Codex's (~/.codex/sessions/YYYY/MM/DD/rollout-<time>-<id>.jsonl).
//
// Both formats are internal to their tools and change between releases, so
// parsing is best-effort: unknown entry and block types are skipped.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Block is one displayable piece of a message.
type Block struct {
	Kind      string `json:"kind"` // text | tool_use | tool_result | image
	Text      string `json:"text,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     string `json:"input,omitempty"`
	IsError   bool   `json:"isError,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
	Data      string `json:"data,omitempty"` // base64 image data
	Truncated bool   `json:"truncated,omitempty"`
}

// Message is a normalised user or assistant entry.
type Message struct {
	UUID      string  `json:"uuid"`
	Timestamp string  `json:"timestamp"`
	Role      string  `json:"role"`
	Blocks    []Block `json:"blocks"`
}

// Limits keep responses small enough for a phone.
const (
	MaxTailBytes    = 16 << 20
	MaxTextChars    = 60_000
	MaxToolChars    = 4_000
	MaxInputChars   = 2_000
	MaxImageB64Size = 4 << 20
)

var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ErrNotFound is returned when no log exists for the session.
var ErrNotFound = errors.New("transcript not found")

// Locate finds <projectsDir>/*/<sessionID>.jsonl.
func Locate(projectsDir, sessionID string) (string, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return "", fmt.Errorf("invalid session id")
	}
	matches, err := filepath.Glob(filepath.Join(projectsDir, "*", sessionID+".jsonl"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", ErrNotFound
	}
	return newest(matches), nil
}

// newest returns the most recently modified of paths, so a session that was
// moved or copied resolves to its live log.
func newest(paths []string) string {
	best, bestTime := "", int64(-1)
	for _, m := range paths {
		if fi, err := os.Stat(m); err == nil && fi.ModTime().UnixNano() > bestTime {
			best, bestTime = m, fi.ModTime().UnixNano()
		}
	}
	return best
}

// Size returns the log size, used by the UI to detect changes cheaply.
func Size(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return fi.Size()
}

// Loader reads the tail of a session log and returns at most limit messages
// (0 = all in the tail) and the log size. Load and LoadCodex are Loaders.
type Loader func(path string, limit int) ([]Message, int64, error)

// Load parses the tail of a Claude Code log and returns at most limit messages.
func Load(path string, limit int) ([]Message, int64, error) {
	return loadTail(path, limit, Parse)
}

// loadTail reads at most MaxTailBytes from the end of the log and parses it
// with parse; the format-specific part is only the parser.
func loadTail(path string, limit int, parse func([]byte) []Message) ([]Message, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := fi.Size()
	start := int64(0)
	if size > MaxTailBytes {
		start = size - MaxTailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxTailBytes))
	if err != nil {
		return nil, 0, err
	}
	if start > 0 {
		// Drop the partial first line.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	msgs := parse(data)
	if limit > 0 && len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	return msgs, size, nil
}

type entry struct {
	Type        string          `json:"type"`
	UUID        string          `json:"uuid"`
	Timestamp   string          `json:"timestamp"`
	IsSidechain bool            `json:"isSidechain"`
	IsMeta      bool            `json:"isMeta"`
	Message     json.RawMessage `json:"message"`
}

type apiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type rawBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	IsError bool            `json:"is_error"`
	Content json.RawMessage `json:"content"`
	Source  *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
}

var allowedImageTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

// Parse converts JSONL bytes into messages, skipping everything that is not
// a main-thread user or assistant message.
func Parse(data []byte) []Message {
	var out []Message
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), MaxTailBytes)
	for sc.Scan() {
		var e entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		if (e.Type != "user" && e.Type != "assistant") || e.IsSidechain || e.IsMeta || len(e.Message) == 0 {
			continue
		}
		var m apiMessage
		if err := json.Unmarshal(e.Message, &m); err != nil {
			continue
		}
		blocks := parseContent(m.Content)
		if len(blocks) == 0 {
			continue
		}
		out = append(out, Message{UUID: e.UUID, Timestamp: e.Timestamp, Role: e.Type, Blocks: blocks})
	}
	return out
}

func parseContent(raw json.RawMessage) []Block {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []Block{textBlock("text", s, MaxTextChars)}
	}
	var rbs []rawBlock
	if json.Unmarshal(raw, &rbs) != nil {
		return nil
	}
	var out []Block
	for _, rb := range rbs {
		switch rb.Type {
		case "text":
			if strings.TrimSpace(rb.Text) != "" {
				out = append(out, textBlock("text", rb.Text, MaxTextChars))
			}
		case "tool_use":
			out = append(out, toolUseBlock(rb.Name, compactJSON(rb.Input)))
		case "tool_result":
			out = append(out, toolResult(rb)...)
		case "image":
			if b, ok := imageBlock(rb); ok {
				out = append(out, b)
			}
		}
	}
	return out
}

func toolResult(rb rawBlock) []Block {
	var s string
	if json.Unmarshal(rb.Content, &s) == nil {
		return []Block{resultBlock(s, rb.IsError)}
	}
	var inner []rawBlock
	if json.Unmarshal(rb.Content, &inner) != nil {
		return nil
	}
	var texts []string
	var out []Block
	for _, ib := range inner {
		switch ib.Type {
		case "text":
			texts = append(texts, ib.Text)
		case "image":
			if b, ok := imageBlock(ib); ok {
				out = append(out, b)
			}
		}
	}
	return append([]Block{resultBlock(strings.Join(texts, "\n"), rb.IsError)}, out...)
}

func imageBlock(rb rawBlock) (Block, bool) {
	if rb.Source == nil || rb.Source.Type != "base64" || !allowedImageTypes[rb.Source.MediaType] {
		return Block{}, false
	}
	if len(rb.Source.Data) > MaxImageB64Size {
		return Block{Kind: "image", MediaType: rb.Source.MediaType, Truncated: true}, true
	}
	return Block{Kind: "image", MediaType: rb.Source.MediaType, Data: rb.Source.Data}, true
}

func textBlock(kind, s string, limit int) Block {
	t, cut := truncate(s, limit)
	return Block{Kind: kind, Text: t, Truncated: cut}
}

// toolUseBlock builds a tool_use block with the input truncated for the UI.
func toolUseBlock(name, input string) Block {
	b := Block{Kind: "tool_use", Name: name}
	b.Input, b.Truncated = truncate(input, MaxInputChars)
	return b
}

// resultBlock builds a tool_result block.
func resultBlock(text string, isErr bool) Block {
	b := textBlock("tool_result", text, MaxToolChars)
	b.IsError = isErr
	return b
}

func truncate(s string, limit int) (string, bool) {
	r := []rune(s)
	if len(r) <= limit {
		return s, false
	}
	return string(r[:limit]), true
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}
