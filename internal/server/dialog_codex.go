package server

import (
	"regexp"
	"strings"
)

// Codex draws approvals, the folder-trust prompt and request_user_input
// questions as a numbered list with a "›" cursor (at column 0 or indented),
// above a key-hint line naming Enter and Esc, at the bottom of the screen:
//
//	  Would you like to run the following command?
//
//	  Environment: local
//
//	  $ echo hi > hello.txt
//
//
//	› 1. Yes, proceed (y)
//	  2. No, and tell Codex what to do differently (esc)
//
//	  Press enter to confirm or esc to cancel
//
//	  Question 1/2 (2 unanswered)
//	  Pick a color
//
//	  › 1. Red                Choose red.
//	    2. Green              Choose green.
//
//	  tab to add notes | enter to submit answer | ←/→ to navigate questions | esc to interrupt
//
// The history above is made of cells that start at column 0 ("• Ran …",
// "› user prompt", "■ …"); everything in the view is indented. Between the
// last history cell and the view there are at least two blank lines; inside the
// view blank lines come singly, except the pair right above the list.
// Anchoring on the hint at the bottom keeps user prompts like "› 1. foo" and
// the agent's own numbered lists in the history from being read as options.

var codexOptionRe = regexp.MustCompile(`^\s*(›)?\s*(\d{1,2})\.\s+(\S.*?)\s*$`)

var wideGapRe = regexp.MustCompile(`\s{2,}`)

// Whole words only: "center" or "description" must not pass for a key hint.
var (
	hintEnterRe = regexp.MustCompile(`(?i)\benter\b`)
	hintEscRe   = regexp.MustCompile(`(?i)\besc\b`)
)

// ParseCodexDialog extracts the selection view at the bottom of a Codex
// screen. It returns nil unless a key hint ends the screen and exactly one
// option of the list above it carries the cursor.
func ParseCodexDialog(screen string) *Dialog {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")

	hint := len(lines) - 1
	for hint >= 0 && isBlank(lines[hint]) {
		hint--
	}
	if hint < 0 {
		return nil
	}
	if !hintEnterRe.MatchString(lines[hint]) || !hintEscRe.MatchString(lines[hint]) || codexOptionRe.MatchString(lines[hint]) {
		return nil
	}

	// The list block: the run of non-blank lines directly above the hint.
	end := hint - 1
	for end >= 0 && isBlank(lines[end]) {
		end--
	}
	if end < 0 {
		return nil
	}
	begin := end
	for begin > 0 && !isBlank(lines[begin-1]) {
		begin--
	}

	type opt struct {
		item  DialogItem
		extra []string
	}
	var opts []opt
	cursor, cursors := -1, 0
	var pre []string
	for i := begin; i <= end; i++ {
		if m := codexOptionRe.FindStringSubmatch(lines[i]); m != nil {
			if m[1] != "" {
				cursors++
				cursor = len(opts)
			}
			opts = append(opts, opt{item: DialogItem{Number: m[2], Label: m[3]}})
			continue
		}
		s := strings.TrimSpace(lines[i])
		if len(opts) == 0 {
			pre = append(pre, s)
		} else {
			last := &opts[len(opts)-1]
			last.extra = append(last.extra, s)
		}
	}
	if len(opts) == 0 || cursors != 1 {
		return nil
	}

	// Descriptions on the same line follow a gap of spaces after the label,
	// but only when every label has one (Codex aligns them in a column).
	split := true
	for _, o := range opts {
		if !wideGapRe.MatchString(o.item.Label) {
			split = false
		}
	}

	d := &Dialog{Cursor: cursor}
	for _, o := range opts {
		it := o.item
		var desc []string
		if split {
			loc := wideGapRe.FindStringIndex(it.Label)
			if rest := strings.TrimSpace(it.Label[loc[1]:]); rest != "" {
				desc = append(desc, rest)
			}
			it.Label = strings.TrimSpace(it.Label[:loc[0]])
		}
		desc = append(desc, o.extra...)
		it.Description = strings.TrimSpace(strings.Join(desc, " "))
		applyCheckbox(&it)
		d.Items = append(d.Items, it)
	}

	// Context: the view's text above the list, without the history cell that
	// ends above it.
	region := 0
	for i := begin - 1; i >= 0; i-- {
		if (!isBlank(lines[i]) && !strings.HasPrefix(lines[i], " ")) || separatorRe.MatchString(lines[i]) {
			region = i + 1
			break
		}
	}
	above := lines[region:begin]
	for i := 0; i+1 < len(above); i++ {
		if !isBlank(above[i]) || !isBlank(above[i+1]) {
			continue
		}
		j := i
		for j < len(above) && isBlank(above[j]) {
			j++
		}
		if j < len(above) {
			above = above[j:]
		}
		break
	}
	for _, l := range above {
		if isRuleOrBlank(l) {
			continue
		}
		d.Context = append(d.Context, strings.TrimSpace(l))
	}
	d.Context = append(d.Context, pre...)
	return d
}

var yesRe = regexp.MustCompile(`^Yes\b`)

// LooksLikeCodexApproval reports whether Enter would approve the visible
// Codex prompt exactly once: the cursor is on "1. Yes…".
func LooksLikeCodexApproval(screen string) bool {
	d := ParseCodexDialog(screen)
	return d != nil && d.Cursor == 0 && d.Items[0].Number == "1" && yesRe.MatchString(d.Items[0].Label)
}

// parseDialogFor reads the selection dialog of the agent's TUI: Codex draws
// a "›" list above a key hint, Claude Code (and anything else) a "❯" list.
func parseDialogFor(agent, screen string) *Dialog {
	if agent == agentCodex {
		return ParseCodexDialog(screen)
	}
	return ParseDialog(screen)
}

// looksLikeApprovalFor reports whether Enter would approve the visible
// permission prompt exactly once.
func looksLikeApprovalFor(agent, screen string) bool {
	if agent == agentCodex {
		return LooksLikeCodexApproval(screen)
	}
	return LooksLikeApprovalDialog(screen)
}
