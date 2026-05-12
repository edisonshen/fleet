package handoff

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// ParseActiveSubagents extracts the `## Active Subagents` section from
// a rendered handoff doc body and returns the list of in-flight worker
// entries. Used by the successor coord on its first turn after handoff
// (issue #93 Phase B2) to know which workers must be re-dispatched.
//
// Section grammar:
//
//	## Active Subagents
//	<body>
//
// Body is one of:
//   - the literal `_(none)_` placeholder (rendered when the outgoing
//     agent had zero live workers) — returns nil, nil.
//   - one or more `- task="..." branch="..." phase="..." agent_id="..."
//     subagent_id="..."` lines — returns the parsed list.
//
// Lines that don't match the entry shape are skipped (logged via the
// returned warnings slice rather than failing the whole parse — a
// malformed entry shouldn't drop the whole resume list).
//
// Returns:
//   - subs:     parsed entries; nil on placeholder body or missing section.
//   - warnings: non-fatal parse issues (malformed lines, unrecognized
//     keys); empty when everything parsed cleanly.
//   - err:      only on hard failures (nil input, scanner error). A
//     missing section is NOT an error — older docs (pre-Phase B2)
//     legitimately have no section; the parser must handle both.
//
// The function is byte-stable across Render(d) round-trips: parse(Render(d))
// returns d.ActiveSubagents (TestParseActiveSubagents_* round-trip tests pin this).
func ParseActiveSubagents(doc []byte) (subs []ActiveSubagent, warnings []string, err error) {
	if doc == nil {
		return nil, nil, fmt.Errorf("handoff: nil doc")
	}

	// Walk lines until we land on the section header. Subagents is the
	// terminal section in v0.2 doc shape (Render appends it last), so
	// once we hit it everything until EOF (or another `## ` heading,
	// for forward-compat with future sections) is the section body.
	scanner := bufio.NewScanner(strings.NewReader(string(doc)))
	// Allow long lines — operator-supplied paths can push the default
	// 64KB buffer in pathological cases. 1MB is comfortably above the
	// hard cap on a handoff doc body.
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	inSection := false
	for scanner.Scan() {
		line := scanner.Text()
		if !inSection {
			if strings.TrimSpace(line) == "## Active Subagents" {
				inSection = true
			}
			continue
		}
		// In section. Stop when we hit another ## heading (forward-
		// compat: a future renderer might append more sections).
		if strings.HasPrefix(line, "## ") {
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if trimmed == ActiveSubagentsNonePlaceholder {
			// Explicit "no live subagents" — return nil so callers can
			// treat empty list and empty placeholder identically.
			return nil, warnings, nil
		}
		entry, ok := parseActiveSubagentLine(trimmed)
		if !ok {
			warnings = append(warnings,
				fmt.Sprintf("active-subagents: skipped malformed line: %q", trimmed))
			continue
		}
		subs = append(subs, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, warnings, fmt.Errorf("handoff: scan: %w", err)
	}
	return subs, warnings, nil
}

// parseActiveSubagentLine parses one body line of the Active Subagents
// section. The line shape is intentionally rigid to avoid a yaml
// dependency. Current (v0.8.3+) shape:
//
//   - task="<slug>" branch="<branch>" phase="<phase>" status="<status>" pr_url="<url>" agent_id="<hex>" subagent_id="<id>"
//
// Legacy shape (pre-v0.8.3, 5 fields) was:
//
//   - task="<slug>" branch="<branch>" phase="<phase>" agent_id="<hex>" subagent_id="<id>"
//
// Each value MUST be Go-strconv.Quote-formatted. Keys may appear in any
// order to tolerate future field additions without churning the parser.
// Unknown keys are skipped silently (forward-compat). Legacy 5-field
// rows parse with status="" and pr_url="" — the successor coord's
// resume logic treats empty pr_url as "no PR yet, re-dispatch Agent".
//
// Returns ok=false on any structural failure (missing leading `- `,
// unparseable quoted-string, etc.); the caller drops the line and
// continues so a single malformed entry doesn't drop the whole list.
func parseActiveSubagentLine(line string) (ActiveSubagent, bool) {
	if !strings.HasPrefix(line, "- ") {
		return ActiveSubagent{}, false
	}
	rest := strings.TrimPrefix(line, "- ")
	pairs, ok := splitKeyValuePairs(rest)
	if !ok {
		return ActiveSubagent{}, false
	}
	var entry ActiveSubagent
	sawAny := false
	for _, p := range pairs {
		switch p.key {
		case "task":
			entry.TaskID = p.value
			sawAny = true
		case "branch":
			entry.Branch = p.value
		case "phase":
			entry.LastPhase = p.value
		case "status":
			entry.Status = p.value
		case "pr_url":
			entry.PRURL = p.value
		case "agent_id":
			entry.AgentID = p.value
		case "subagent_id":
			entry.SubagentID = p.value
		}
	}
	// Require at least one recognized key + a non-empty task ID — an
	// entry that only contained unknown keys, or one whose task is
	// empty, can't drive a re-dispatch decision.
	if !sawAny || entry.TaskID == "" {
		return ActiveSubagent{}, false
	}
	return entry, true
}

type kvPair struct {
	key   string
	value string
}

// splitKeyValuePairs walks `<key>="<value>" <key>="<value>" ...`
// segments. The value MUST be Go-quoted (strconv.Quote-format). Spaces
// outside quoted values are the delimiter. Returns ok=false on any
// quoting / structure failure so the caller can skip the line.
func splitKeyValuePairs(s string) ([]kvPair, bool) {
	var out []kvPair
	for i := 0; i < len(s); {
		// Skip leading whitespace.
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			break
		}
		// Read key up to '='.
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			return nil, false
		}
		key := s[i : i+eq]
		if key == "" {
			return nil, false
		}
		i += eq + 1
		// Value must start with '"'. strconv.Unquote handles the rest.
		if i >= len(s) || s[i] != '"' {
			return nil, false
		}
		// Find the matching closing quote, accounting for backslash
		// escapes inside the string. strconv.Unquote validates the
		// content; we only need to find the boundary.
		end := i + 1
		for end < len(s) {
			if s[end] == '\\' {
				// Skip the escape's next char (covers `\"`, `\\`,
				// `\n`, `\xNN`, etc. — strconv.Unquote validates).
				// Bounds-check the +2 jump so a malformed trailing
				// backslash (`"foo\`) doesn't index past len(s) in
				// the strconv.Unquote call below — return false
				// instead and let the caller drop the line.
				if end+1 >= len(s) {
					return nil, false
				}
				end += 2
				continue
			}
			if s[end] == '"' {
				break
			}
			end++
		}
		if end >= len(s) {
			return nil, false
		}
		quoted := s[i : end+1]
		val, err := strconv.Unquote(quoted)
		if err != nil {
			return nil, false
		}
		out = append(out, kvPair{key: key, value: val})
		i = end + 1
	}
	return out, true
}
