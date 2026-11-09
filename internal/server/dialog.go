package server

import (
	"regexp"
	"strings"
)

// Dialog is a Claude Code selection dialog read from the terminal screen:
// permission prompts ("Do you want to proceed?"), AskUserQuestion pages
// (single and multi select), and their review page.
//
// Every such dialog is a list with a "❯" cursor. Choosing item i means
// moving the cursor with up/down and pressing Enter; that is all the server
// ever sends for it. The struct is shared with the Codex parser
// (ParseCodexDialog), whose lists use a "›" cursor.
type Dialog struct {
	Context []string     `json:"context"` // question / command text above the options
	Items   []DialogItem `json:"items"`
	Cursor  int          `json:"cursor"` // index into Items, -1 if not found
}

// DialogItem is one selectable line.
type DialogItem struct {
	Number      string `json:"number,omitempty"` // "1", "2", … ("" for Submit)
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Checked     *bool  `json:"checked,omitempty"`  // multi-select checkbox state
	FreeText    bool   `json:"freeText,omitempty"` // "Type something": needs typing in the terminal
}

var (
	separatorRe = regexp.MustCompile(`^\s*─{10,}\s*$`)
	dashRuleRe  = regexp.MustCompile(`^\s*[╌┄-]{10,}\s*$`)
	numberedRe  = regexp.MustCompile(`^\s*(❯)?\s*(\d{1,2})\.\s+(.*?)\s*$`)
	submitRe    = regexp.MustCompile(`^\s*(❯)?\s+(Submit)\s*$`)
	checkboxRe  = regexp.MustCompile(`^\[(.)\]\s*(.*)$`)
	freeTextRe  = regexp.MustCompile(`(?i)^type something\.?$`)
)

func isFooter(s string) bool {
	return strings.Contains(s, "Esc to cancel") || strings.Contains(s, "Enter to select") ||
		strings.Contains(s, "Enter to confirm")
}

func isBlank(s string) bool { return strings.TrimSpace(s) == "" }

// isRuleOrBlank reports a line that carries no dialog text: a blank line,
// a separator or a dashed rule.
func isRuleOrBlank(s string) bool {
	return isBlank(s) || separatorRe.MatchString(s) || dashRuleRe.MatchString(s)
}

// isPromptEcho reports a line of the user's own input history ("❯ text" at
// column 0) which sits above the dialog and must not be read as an option.
func isPromptEcho(s string) bool {
	if !strings.HasPrefix(s, "❯") {
		return false
	}
	return !numberedRe.MatchString(s) && !submitRe.MatchString(s)
}

type optionLine struct {
	line   int
	cursor bool
	item   DialogItem
}

// applyCheckbox moves a leading "[x]" / "[ ]" of the label into Checked.
func applyCheckbox(it *DialogItem) {
	if cb := checkboxRe.FindStringSubmatch(it.Label); cb != nil {
		checked := cb[1] != " "
		it.Checked = &checked
		it.Label = cb[2]
	}
}

func parseOption(s string) (optionLine, bool) {
	if m := numberedRe.FindStringSubmatch(s); m != nil {
		it := DialogItem{Number: m[2], Label: m[3]}
		applyCheckbox(&it)
		it.FreeText = freeTextRe.MatchString(it.Label)
		return optionLine{cursor: m[1] != "", item: it}, true
	}
	if m := submitRe.FindStringSubmatch(s); m != nil {
		return optionLine{cursor: m[1] != "", item: DialogItem{Label: "Submit"}}, true
	}
	return optionLine{}, false
}

// ParseDialog extracts the selection dialog at the bottom of the screen.
// It returns nil when no option list is visible.
func ParseDialog(screen string) *Dialog {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")

	// Only look below the last line of the user's own input history.
	floor := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if isPromptEcho(lines[i]) {
			floor = i + 1
			break
		}
	}

	var opts []optionLine
	for i := floor; i < len(lines); i++ {
		if o, ok := parseOption(lines[i]); ok {
			o.line = i
			opts = append(opts, o)
		}
	}
	if len(opts) == 0 {
		return nil
	}

	// The dialog starts at the separator above the cursor. A separator whose
	// previous line is itself an option is inside the dialog (AskUserQuestion
	// puts one before "Chat about this"), so keep walking up past it.
	cursorLine := -1
	for _, o := range opts {
		if o.cursor {
			cursorLine = o.line
		}
	}
	if cursorLine < 0 {
		cursorLine = opts[0].line
	}
	start := floor
	for i := cursorLine - 1; i >= floor; i-- {
		if !separatorRe.MatchString(lines[i]) {
			continue
		}
		j := i - 1
		for j >= floor && isBlank(lines[j]) {
			j--
		}
		if j >= floor {
			if _, ok := parseOption(lines[j]); ok {
				continue
			}
		}
		start = i + 1
		break
	}
	kept := opts[:0]
	for _, o := range opts {
		if o.line >= start {
			kept = append(kept, o)
		}
	}
	opts = kept
	if len(opts) == 0 {
		return nil
	}

	d := &Dialog{Cursor: -1}
	for i := start; i < opts[0].line; i++ {
		s := strings.TrimRight(lines[i], " ")
		if isRuleOrBlank(s) || isFooter(s) {
			continue
		}
		d.Context = append(d.Context, strings.TrimSpace(s))
	}
	for k, o := range opts {
		end := len(lines)
		if k+1 < len(opts) {
			end = opts[k+1].line
		}
		var desc []string
		for i := o.line + 1; i < end; i++ {
			s := strings.TrimSpace(lines[i])
			if s == "" || separatorRe.MatchString(lines[i]) || isFooter(s) {
				break
			}
			desc = append(desc, s)
		}
		o.item.Description = strings.Join(desc, " ")
		if o.item.Description == o.item.Label {
			o.item.Description = ""
		}
		if o.cursor {
			d.Cursor = k
		}
		d.Items = append(d.Items, o.item)
	}
	return d
}

// KeysToChoose returns the key presses that select item index from the
// current cursor: up/down moves followed by Enter.
func (d *Dialog) KeysToChoose(index int) []string {
	var keys []string
	step, n := "down", index-d.Cursor
	if n < 0 {
		step, n = "up", -n
	}
	for i := 0; i < n; i++ {
		keys = append(keys, step)
	}
	return append(keys, "enter")
}
