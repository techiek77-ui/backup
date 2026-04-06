package regexrating

import (
	"strings"
)

// Safety is the result of [Analyze].
type Safety struct {
	// AtStart is true when prepending \b to the pattern is safe.
	AtStart bool
	// AtEnd is true when appending \b to the pattern is safe.
	AtEnd bool
	// Reason contains a human-readable explanation for any unsafe verdict.
	// Empty when both AtStart and AtEnd are true.
	Reason string
}

// Analyze inspects pattern and returns whether \b is safe at the start and/or
// end. It strips common leading inline flags ((?i), (?im), …) before analysis
// so callers can pass raw patterns or patterns prefixed with (?i).
func Analyze(pattern string) Safety {
	core := stripLeadingFlags(pattern)
	safeStart, reasonStart := checkStart(core)
	safeEnd, reasonEnd := checkEnd(core)

	var parts []string
	if !safeStart {
		parts = append(parts, "start: "+reasonStart)
	}
	if !safeEnd {
		parts = append(parts, "end: "+reasonEnd)
	}
	return Safety{
		AtStart: safeStart,
		AtEnd:   safeEnd,
		Reason:  strings.Join(parts, "; "),
	}
}

// WrapIfSafe wraps pattern with \b anchors only where the analysis says it is
// safe, and only when the anchor is not already present.
func WrapIfSafe(pattern string) string {
	isSafe := Analyze(pattern)
	result := pattern
	core := stripLeadingFlags(pattern)

	if isSafe.AtStart && !strings.HasPrefix(core, `\b`) && !strings.HasPrefix(core, `^`) {
		result = `\b` + result
	}
	if isSafe.AtEnd && !strings.HasSuffix(core, `\b`) && !strings.HasSuffix(core, `$`) {
		result = result + `\b`
	}
	return result
}

// ─────────────────────────────────────────────────────────────────────────────
// Start-of-pattern analysis
// ─────────────────────────────────────────────────────────────────────────────

// checkStart returns whether \b can be safely prepended to pattern.
func checkStart(pattern string) (bool, string) {
	if pattern == "" {
		return false, "empty pattern"
	}

	// Top-level alternation — cannot safely determine the first token.
	if alts := splitTopLevelAlternatives(pattern); len(alts) > 1 {
		return false, "pattern contains top-level alternation"
	}

	// Already has a word-boundary or caret anchor.
	if strings.HasPrefix(pattern, `\b`) {
		return false, `already starts with \b`
	}
	if strings.HasPrefix(pattern, `^`) {
		return false, "already anchored at start with ^"
	}

	// Negated character class: [^…]
	if strings.HasPrefix(pattern, "[^") {
		return false, "starts with negated character class [^…]"
	}

	// Backslash escape sequence.
	if len(pattern) >= 2 && pattern[0] == '\\' {
		// Detect Unicode category \p{L} etc.
		if pattern[1] == 'p' && len(pattern) >= 4 && pattern[2] == '{' {
			idx := strings.Index(pattern, "}")
			if idx > 2 {
				cat := pattern[3:idx]
				if strings.HasPrefix(cat, "L") || strings.HasPrefix(cat, "N") {
					return true, ""
				}
				return false, "starts with non-word Unicode category \\p{" + cat + "}"
			}
		}
		return checkEscapeForWordChar(pattern[1], "starts with")
	}

	// Group constructs: (?…) capturing or non-capturing.
	if pattern[0] == '(' {
		return checkGroupStart(pattern)
	}

	// Non-negated character class: […]
	if pattern[0] == '[' {
		ok := classContainsWordChar(pattern)
		if ok {
			return true, ""
		}
		return false, "character class at start contains no word characters"
	}

	// Dot — matches any character including non-word characters.
	if pattern[0] == '.' {
		return false, "starts with . (matches any character)"
	}

	// Literal rune.
	r := []rune(pattern)[0]
	if isWordRune(r) {
		return true, ""
	}
	return false, "starts with non-word literal character"
}

// checkGroupStart analyses a pattern that begins with '('.
func checkGroupStart(pattern string) (bool, string) {
	if len(pattern) < 2 {
		return false, "starts with incomplete group"
	}

	if pattern[1] != '?' {
		// Plain capturing group — content is too complex to analyze statically.
		return false, "starts with capturing group"
	}

	if len(pattern) < 3 {
		return false, "starts with incomplete (?…) group"
	}

	inner := pattern[2:]

	switch {
	// Lookahead / lookbehind: (?=…) (?!…) (?<=…) (?<!…)
	case strings.HasPrefix(inner, "="),
		strings.HasPrefix(inner, "!"),
		strings.HasPrefix(inner, "<="),
		strings.HasPrefix(inner, "<!"):
		return false, "starts with lookahead or lookbehind"

	// Non-capturing group (?:…)
	case strings.HasPrefix(inner, ":"):
		// Detect (?:^|…) — start-of-string alternation.
		content := inner[1:]
		if strings.HasPrefix(content, "^") {
			return false, "starts with (?:^|…) — start-of-string alternation"
		}
		// Check if any top-level alternative begins with ^ or \b.
		alts := splitTopLevelAlternatives(content[:indexOfClosingParen(content)])
		for _, alt := range alts {
			altTrimmed := stripLeadingFlags(strings.TrimSpace(alt))
			if strings.HasPrefix(altTrimmed, "^") || strings.HasPrefix(altTrimmed, `\b`) {
				return false, "non-capturing group contains a start-anchor alternative"
			}
		}
		if len(alts) > 0 {
			return checkStart(strings.TrimSpace(alts[0]))
		}
		return false, "starts with non-capturing group (?:…) — cannot determine first token"

	default:
		return false, "starts with group construct (?…)"
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// End-of-pattern analysis
// ─────────────────────────────────────────────────────────────────────────────

// checkEnd returns whether \b can be safely appended to pattern.
func checkEnd(pattern string) (bool, string) {
	if pattern == "" {
		return false, "empty pattern"
	}

	// Top-level alternation — cannot safely determine the last token.
	if alts := splitTopLevelAlternatives(pattern); len(alts) > 1 {
		return false, "pattern contains top-level alternation"
	}

	// Already has a word-boundary or dollar anchor.
	if strings.HasSuffix(pattern, `\b`) {
		return false, `already ends with \b`
	}
	if strings.HasSuffix(pattern, "$") {
		return false, "already anchored at end with $"
	}

	// Strip trailing quantifier(s) to expose the last atom.
	stripped := stripTrailingQuantifier(pattern)
	if stripped == "" {
		return false, "empty after stripping quantifier"
	}

	last := stripped[len(stripped)-1]

	switch {
	case last == ']':
		return checkClassEnd(stripped)

	case last == ')':
		return checkGroupEnd(stripped)

	case last == '\\':
		// Trailing backslash — invalid pattern; treat as unsafe.
		return false, "pattern ends with bare backslash"

	default:
		// Check for a two-character backslash escape at the end, e.g. \w \d \n
		// Guard against a literal \\ where the backslash itself is escaped.
		if len(stripped) >= 2 && stripped[len(stripped)-2] == '\\' && !isEscaped(stripped, len(stripped)-2) {
			return checkEscapeForWordChar(stripped[len(stripped)-1], "ends with")
		}

		// Check for Unicode category at end: \p{L} \p{N} \P{…}
		if escape, pos := findTrailingUnicodeEscape(stripped); pos >= 0 {
			cat := escape[3:len(escape)-1]
			if escape[1] == 'p' && (strings.HasPrefix(cat, "L") || strings.HasPrefix(cat, "N")) {
				return true, ""
			}
			return false, "ends with non-word Unicode category " + escape
		}

		// Literal rune.
		r := []rune(stripped)[len([]rune(stripped))-1]
		if isWordRune(r) {
			return true, ""
		}
		return false, "ends with non-word literal character"
	}
}

// checkClassEnd analyses a pattern whose (quantifier-stripped) tail is ']'.
func checkClassEnd(pattern string) (bool, string) {
	open := findMatchingOpenBracket(pattern)
	if open < 0 {
		return false, "could not find matching [ for ]"
	}
	// Negated class: [^…]
	if open+1 < len(pattern) && pattern[open+1] == '^' {
		return false, "ends with negated character class [^…]"
	}
	classBody := pattern[open+1 : len(pattern)-1]
	if classContainsWordCharBody(classBody) {
		return true, ""
	}
	return false, "character class at end contains no word characters"
}

// checkGroupEnd analyses a pattern whose (quantifier-stripped) tail is ')'.
func checkGroupEnd(pattern string) (bool, string) {
	open := findMatchingOpenParen(pattern)
	if open < 0 {
		return false, "could not find matching ( for )"
	}
	groupBody := pattern[open+1 : len(pattern)-1]

	// Detect (?:…|$) — end-of-string alternation.
	altsForDollar := splitTopLevelAlternatives(groupBody)
	for _, alt := range altsForDollar {
		trimmed := strings.TrimSpace(alt)
		n := len(trimmed)
		if n > 0 && trimmed[n-1] == '$' && !isEscaped(trimmed, n-1) {
			return false, "ends with (?:…|$) — end-of-string alternation"
		}
	}

	// Strip non-capturing/flag prefix from group body.
	inner := groupBody
	if strings.HasPrefix(inner, "?=") || strings.HasPrefix(inner, "?!") ||
		strings.HasPrefix(inner, "?<=") || strings.HasPrefix(inner, "?<!") {
		return false, "ends with lookahead or lookbehind group"
	} else if strings.HasPrefix(inner, "?:") {
		inner = inner[2:]
	} else if strings.HasPrefix(inner, "?") {
		// Flag-setting group like (?i:…) — strip up to ':'.
		if colon := strings.Index(inner, ":"); colon >= 0 {
			inner = inner[colon+1:]
		}
	}

	// All top-level alternatives must end on a word character.
	alts := splitTopLevelAlternatives(inner)
	for _, alt := range alts {
		ok, reason := checkEnd(alt)
		if !ok {
			return false, "group at end: " + reason
		}
	}
	return true, ""
}

// ─────────────────────────────────────────────────────────────────────────────
// Shared helpers
// ─────────────────────────────────────────────────────────────────────────────

// checkEscapeForWordChar returns whether the character c (immediately after a
// backslash) represents a word character. direction is used in the reason string.
func checkEscapeForWordChar(c byte, direction string) (bool, string) {
	switch c {
	case 'w', 'd':
		return true, ""
	case 'W', 'D', 'S', 's', 'B', 'b', 'n', 'r', 't', 'f', 'v', 'a':
		return false, direction + ` non-word or whitespace escape \` + string(c)
	case 'A', 'Z', 'z', 'k', 'g', 'G':
		return false, direction + ` anchor or backreference escape \` + string(c)
	case 'p', 'P':
		return false, direction + ` unparsed Unicode category \` + string(c)
	default:
		// Escaped literal — safe if it's a word rune.
		r := rune(c)
		if isWordRune(r) {
			return true, ""
		}
		return false, direction + ` non-word escaped literal \` + string(c)
	}
}

// stripLeadingFlags removes leading inline flag groups such as (?i), (?im),
// (?imsx), (?-i), etc. Handles multiple consecutive flag groups.
func stripLeadingFlags(p string) string {
	for {
		if !strings.HasPrefix(p, "(?") {
			break
		}
		// Scan chars after (? looking for flags then ')'.
		i := 2
		for i < len(p) && (strings.ContainsRune("imsxU!<=-", rune(p[i])) || (p[i] >= 'a' && p[i] <= 'z') || p[i] == '-') {
			// Allow flag characters; stop at ')' or anything unexpected.
			ch := p[i]
			if ch == ')' {
				break
			}
			// Stop if we hit a character that is clearly not a flag.
			if !strings.ContainsRune("imsx-U", rune(ch)) {
				break
			}
			i++
		}
		if i < len(p) && p[i] == ')' {
			p = p[i+1:]
		} else {
			break
		}
	}
	return p
}

// stripTrailingQuantifier removes a single trailing quantifier
// (+, *, ?, {n}, {n,m}) and an optional trailing lazy/possessive modifier (? or +)
// from pattern, returning the atom that the quantifier applied to.
func stripTrailingQuantifier(pattern string) string {
	n := len(pattern)
	if n == 0 {
		return pattern
	}

	i := n - 1

	// Optional lazy/possessive modifier after a quantifier symbol.
	hasModifier := false
	if i > 0 && (pattern[i] == '?' || pattern[i] == '+') {
		prev := pattern[i-1]
		if prev == '+' || prev == '*' || prev == '?' || prev == '}' {
			hasModifier = true
			i--
		}
	}

	switch pattern[i] {
	case '+', '*':
		return pattern[:i]
	case '?':
		// Bare '?' — 0-or-1 quantifier (possessive/lazy already stripped above).
		return pattern[:i]
	case '}':
		// Repetition {n} or {n,m}: find matching '{'.
		j := i - 1
		for j >= 0 && pattern[j] != '{' {
			j--
		}
		if j >= 0 && pattern[j] == '{' {
			// Verify what's inside is numeric/comma (guard against false positives).
			inside := pattern[j+1 : i]
			validQuantifier := len(inside) > 0
			for _, ch := range inside {
				if ch != ',' && !(ch >= '0' && ch <= '9') {
					validQuantifier = false
					break
				}
			}
			if validQuantifier && j > 0 {
				return pattern[:j]
			}
		}
		// Not a repetition string (e.g. \p{L}).
		// If we stripped a trailing '+' or '?' thinking it was a modifier,
		// it was actually the primary quantifier. Strip it.
		if hasModifier {
			return pattern[:n-1]
		}
	}
	return pattern
}

// findMatchingOpenParen scans backwards from the last ')' in pattern and
// returns the index of its matching '(', accounting for nesting and escape
// sequences. Returns -1 if not found.
func findMatchingOpenParen(pattern string) int {
	n := len(pattern)
	if n == 0 || pattern[n-1] != ')' {
		return -1
	}
	depth := 0
	for i := n - 1; i >= 0; i-- {
		if isEscaped(pattern, i) {
			continue
		}
		switch pattern[i] {
		case ')':
			depth++
		case '(':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// findMatchingOpenBracket scans backwards from the last ']' in pattern and
// returns the index of its matching '['. Returns -1 if not found.
func findMatchingOpenBracket(pattern string) int {
	n := len(pattern)
	if n == 0 || pattern[n-1] != ']' {
		return -1
	}
	// Walk backwards; character classes don't nest in standard regex.
	for i := n - 2; i >= 0; i-- {
		if isEscaped(pattern, i) {
			continue
		}
		if pattern[i] == '[' {
			return i
		}
	}
	return -1
}

// isEscaped reports whether the character at pos in pattern is preceded by an
// odd number of backslashes.
func isEscaped(pattern string, pos int) bool {
	count := 0
	for i := pos - 1; i >= 0 && pattern[i] == '\\'; i-- {
		count++
	}
	return count%2 == 1
}

// indexOfClosingParen returns the index of the first unmatched ')' in s
// (i.e. the ')' that closes an opening '(' assumed to be before s).
// Returns len(s) if not found.
func indexOfClosingParen(s string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		if isEscaped(s, i) {
			continue
		}
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return len(s)
}

// splitTopLevelAlternatives splits pattern at top-level '|' characters
// (i.e. not inside groups or character classes).
func splitTopLevelAlternatives(pattern string) []string {
	var alts []string
	depth := 0
	start := 0
	inClass := false

	for i := 0; i < len(pattern); i++ {
		if isEscaped(pattern, i) {
			continue
		}
		ch := pattern[i]
		switch {
		case ch == '[' && !inClass:
			inClass = true
		case ch == ']' && inClass:
			inClass = false
		case ch == '(' && !inClass:
			depth++
		case ch == ')' && !inClass:
			depth--
		case ch == '|' && depth == 0 && !inClass:
			alts = append(alts, pattern[start:i])
			start = i + 1
		}
	}
	alts = append(alts, pattern[start:])
	return alts
}

// classContainsWordChar determines whether the character class starting at the
// beginning of pattern (i.e. pattern[0] == '[') contains ONLY word characters.
// If the class accepts any non-word character it returns false.
func classContainsWordChar(pattern string) bool {
	// Find the closing ']'.
	end := 1
	for end < len(pattern) {
		if pattern[end] == '\\' && end+1 < len(pattern) {
			end += 2
			continue
		}
		if pattern[end] == ']' {
			break
		}
		end++
	}
	body := pattern[1:end]
	// Negated class — handled by caller, but guard here too.
	if strings.HasPrefix(body, "^") {
		return false
	}
	return classContainsWordCharBody(body)
}

// classContainsWordCharBody returns true only when EVERY element in the
// character class body is a word-character construct AND at least one is
// present. Any non-word escape, punctuation literal, or whitespace causes a
// false result so that \b is never incorrectly added around mixed classes.
//
// Recognised word elements: \w  \d  \p{L…}  \p{N…}  [A-Z]  [a-z]  [0-9]  _
// Non-word triggers:  \s \S \W \D \n \r \t \f \v \- (escaped hyphen)
//
//	any literal character outside [A-Za-z0-9_]
//	a bare '-' at the very start or end of the body
func classContainsWordCharBody(body string) bool {
	hasWord := false
	i := 0
	for i < len(body) {
		ch := body[i]
		if ch == '\\' && i+1 < len(body) {
			next := body[i+1]
			switch next {
			case 'w', 'd':
				hasWord = true
				i += 2
			case 'p':
				// \p{L} \p{N} — Unicode letter / number: treat as word.
				// Skip to end of {category}.
				i += 2
				catStart := i
				if i < len(body) && body[i] == '{' {
					i++
					for i < len(body) && body[i] != '}' {
						i++
					}
					if i < len(body) && body[i] == '}' {
						cat := body[catStart+1 : i]
						if strings.HasPrefix(cat, "L") || strings.HasPrefix(cat, "N") {
							hasWord = true
						} else {
							return false
						}
						i++ // consume '}'
					} else {
						return false
					}
				} else {
					return false
				}
			default:
				// \s \S \W \D \n \r \t \f \v or \- (escaped hyphen) —
				// all are non-word or ambiguous.  Also covers any other
				// escape that does not represent a word character.
				return false
			}
		} else if ch == '-' {
			// Bare hyphen:
			//   • At start or end of class body → literal hyphen (non-word).
			//   • In the middle → range separator between the adjacent chars,
			//     which are checked individually.
			if i == 0 || i == len(body)-1 {
				return false
			}
			i++ // range separator — surrounding chars validated separately
		} else if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '_' {
			hasWord = true
			i++
		} else {
			// Any other literal (space, dot, @, %, +, |, …) is non-word.
			return false
		}
	}
	return hasWord
}

// findTrailingUnicodeEscape looks for a \p{…} or \P{…} sequence at the very
// end of pattern. Returns (escape, startPos) or ("", -1).
func findTrailingUnicodeEscape(pattern string) (string, int) {
	n := len(pattern)
	if n < 4 || pattern[n-1] != '}' {
		return "", -1
	}
	// Find matching '{'.
	i := n - 2
	for i >= 0 && pattern[i] != '{' {
		i--
	}
	if i < 2 {
		return "", -1
	}
	// Expect \p or \P immediately before '{'.
	if pattern[i-1] != 'p' && pattern[i-1] != 'P' {
		return "", -1
	}
	if i < 2 || pattern[i-2] != '\\' {
		return "", -1
	}
	return pattern[i-2:], i - 2
}

// isWordRune reports whether r is an ASCII regex word character: [A-Za-z0-9_].
// This intentionally uses ASCII-only matching to stay consistent with Go's \b
// semantics (which treat only [0-9A-Za-z_] as word characters).
func isWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
}
