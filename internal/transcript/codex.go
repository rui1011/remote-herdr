package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
)

// LocateCodex finds <sessionsDir>/YYYY/MM/DD/rollout-<time>-<sessionID>.jsonl,
// the log Codex writes for a session. sessionID is Codex's thread id: herdr
// reports it, and Codex exports it to commands as $CODEX_THREAD_ID.
// sessionsDir must be absolute; an empty or relative one finds nothing.
func LocateCodex(sessionsDir, sessionID string) (string, error) {
	if !filepath.IsAbs(sessionsDir) {
		// An empty or relative dir would make the glob relative to the
		// process cwd.
		return "", ErrNotFound
	}
	return locateNewest(sessionID, sessionsDir, "*", "*", "*", "rollout-*-"+sessionID+".jsonl")
}

// LoadCodex parses the tail of a Codex rollout and returns at most limit messages.
func LoadCodex(path string, limit int) ([]Message, int64, error) {
	return loadTail(path, limit, ParseCodex)
}

type codexLine struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type string          `json:"type"`
		Item json.RawMessage `json:"item"`
	} `json:"payload"`
}

// codexItem is one item of a rollout. Polymorphic fields stay raw so that one
// odd field (e.g. "result" is a string for image generation but an object for
// MCP calls) never drops the whole item.
type codexItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Text             string          `json:"text"`
	Command          json.RawMessage `json:"command"`
	AggregatedOutput string          `json:"aggregated_output"`
	ExitCode         *int            `json:"exit_code"`
	Status           string          `json:"status"`
	Changes          map[string]struct {
		Type        string  `json:"type"`
		Content     string  `json:"content"`
		UnifiedDiff string  `json:"unified_diff"`
		MovePath    *string `json:"move_path"`
	} `json:"changes"`
	Stderr looseString `json:"stderr"`
	Kind   string      `json:"kind"`
	Query  string      `json:"query"`
	Action struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"action"`
	RevisedPrompt string          `json:"revisedPrompt"`
	SavedPath     string          `json:"savedPath"`
	Path          string          `json:"path"`
	Server        string          `json:"server"`
	Tool          string          `json:"tool"`
	Arguments     json.RawMessage `json:"arguments"`
	Result        json.RawMessage `json:"result"`
}

// looseString is a string field that tolerates any other JSON type (left empty).
type looseString string

func (l *looseString) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*l = looseString(s)
	}
	return nil
}

var itemCompleted = []byte(`"item_completed"`)

// ParseCodex converts a Codex rollout into messages. Only the completed items
// of event_msg lines are used: they are what Codex itself shows, while the
// response_item lines duplicate them and also carry injected instructions and
// environment context (and the exec tool's input is JavaScript).
func ParseCodex(data []byte) []Message {
	var out []Message
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), MaxTailBytes)
	for idx := 0; sc.Scan(); idx++ {
		line := sc.Bytes()
		// Rollouts have very large lines that are never needed.
		if !bytes.Contains(line, itemCompleted) {
			continue
		}
		var l codexLine
		if json.Unmarshal(line, &l) != nil || l.Type != "event_msg" || l.Payload.Type != "item_completed" {
			continue
		}
		var it codexItem
		if json.Unmarshal(l.Payload.Item, &it) != nil {
			continue
		}
		blocks := codexBlocks(it)
		if len(blocks) == 0 {
			continue
		}
		role := "assistant"
		if it.Type == "UserMessage" {
			role = "user"
		}
		// The UI keys its list by UUID, so ids must be unique in one response.
		uuid := it.ID
		if uuid == "" || seen[uuid] {
			uuid = fmt.Sprintf("%s#%d", it.ID, idx)
		}
		seen[uuid] = true
		out = append(out, Message{UUID: uuid, Timestamp: l.Timestamp, Role: role, Blocks: blocks})
	}
	return out
}

func codexBlocks(it codexItem) []Block {
	var out []Block
	switch it.Type {
	case "UserMessage", "AgentMessage":
		for _, c := range it.Content {
			// Codex writes "Text" in agent messages, "text" elsewhere.
			if c.Type == "text" || (it.Type == "AgentMessage" && strings.EqualFold(c.Type, "text")) {
				out = appendText(out, c.Text)
			}
		}
	case "Plan":
		out = appendText(out, it.Text)
	case "CommandExecution":
		out = codexCommand(it)
	case "FileChange":
		out = codexFileChange(it)
	case "Extension":
		switch it.Kind {
		case "web.search":
			out = append(out, toolUse("WebSearch", nonEmpty("query", it.Query, "url", it.Action.URL)))
		case "image_gen.generation":
			out = append(out, toolUse("ImageGen", nonEmpty("prompt", it.RevisedPrompt, "path", it.SavedPath)))
		}
	case "ImageView":
		p := it.Path
		if strings.HasPrefix(p, "file://") {
			if u, err := url.Parse(p); err == nil {
				p = u.Path
			}
		}
		out = append(out, toolUse("ViewImage", map[string]string{"path": p}))
	case "McpToolCall":
		out = codexMCP(it)
	}
	return out
}

// codexArgv renders a command, normally an argv array such as
// ["/bin/bash","-lc","echo hi"], the way a person would have typed it.
func codexArgv(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var argv []string
	if json.Unmarshal(raw, &argv) != nil {
		return ""
	}
	if len(argv) == 3 && (argv[1] == "-lc" || argv[1] == "-c") {
		return argv[2]
	}
	return strings.Join(argv, " ")
}

// codexFailed reports whether an item status is neither empty nor completed.
func codexFailed(status string) bool { return status != "" && status != "completed" }

func codexCommand(it codexItem) []Block {
	cmd := codexArgv(it.Command)
	out := []Block{toolUse("Shell", map[string]string{"command": cmd})}
	failed := codexFailed(it.Status) || (it.ExitCode != nil && *it.ExitCode != 0)
	text := it.AggregatedOutput
	if text == "" && failed {
		if it.ExitCode != nil {
			text = fmt.Sprintf("exit code %d", *it.ExitCode)
		} else {
			text = it.Status
		}
	}
	if text != "" || failed {
		out = append(out, resultBlock(text, failed))
	}
	return out
}

func codexFileChange(it codexItem) []Block {
	paths := make([]string, 0, len(it.Changes))
	for p := range it.Changes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []Block
	for _, p := range paths {
		c := it.Changes[p]
		in := map[string]string{"file_path": p, "change": c.Type}
		if c.MovePath != nil && *c.MovePath != "" {
			in["move_path"] = *c.MovePath
		}
		out = append(out, toolUse("Edit", in))
		text := ""
		switch c.Type {
		case "update":
			text = c.UnifiedDiff
		case "add":
			text = c.Content
		}
		if text != "" {
			out = append(out, resultBlock(text, false))
		}
	}
	if codexFailed(it.Status) {
		text := strings.TrimSpace(string(it.Stderr))
		if text == "" {
			text = it.Status
		}
		out = append(out, resultBlock(text, true))
	}
	return out
}

func codexMCP(it codexItem) []Block {
	name := it.Tool
	if it.Server != "" {
		name = it.Server + "." + it.Tool
	}
	out := []Block{toolUseBlock(name, compactJSON(it.Arguments))}

	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(it.Result, &res) // a non-object result simply has no text
	var texts []string
	for _, c := range res.Content {
		if c.Text != "" {
			texts = append(texts, c.Text)
		}
	}
	text := strings.Join(texts, "\n")
	isErr := res.IsError || codexFailed(it.Status)
	if text == "" && isErr {
		text = it.Status
	}
	if text != "" || isErr {
		out = append(out, resultBlock(text, isErr))
	}
	return out
}

// nonEmpty builds a tool input from key/value pairs, leaving out empty values.
func nonEmpty(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			m[kv[i]] = kv[i+1]
		}
	}
	return m
}

// toolUse builds a tool_use block whose input is the map as indented JSON.
// HTML escaping is off because the UI shows the input verbatim.
func toolUse(name string, in map[string]string) Block {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(in) // map[string]string cannot fail; keys come out sorted
	return toolUseBlock(name, strings.TrimRight(buf.String(), "\n"))
}
