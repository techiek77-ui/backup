package regexrating

import (
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// Analyze tests
// ─────────────────────────────────────────────────────────────────────────────

func TestAnalyze_Safe(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		start   bool
		end     bool
	}{
		// \d / \w prefixed / suffixed patterns
		{"digit sequence", `\d{3}-\d{2}-\d{4}`, true, true},
		{"word char wildcard", `\w+`, true, true},
		{"word char optional", `\w?`, true, true},
		{"word char curly", `\d{4}`, true, true},
		{"word char lazy", `\d+?`, true, true},

		// Literal word characters
		{"literal alpha", `[Aa]bc`, true, true},
		{"literal digit class", `[0-9]+`, true, true},
		{"literal underscore suffix", `foo_bar`, true, true},

		// Non-negated character classes with word chars
		{"char class alpha range", `[A-Za-z]+`, true, true},
		{"char class alphanum", `[A-Za-z0-9]{5}`, true, true},
		// \w-only class is safe; mixed classes (\w + hyphen, \d + dot) are not
		{"pure digit class", `[\d]+`, true, true},

		// Flag prefix should be stripped before analysis
		{"(?im) prefix stripped", `(?im)\w+`, true, true},
		{"(?i) on literal word char", `(?i)hello`, true, true},

		// Bug 2: non-capturing group safely starting with word char
		{"non-capturing group safely starting", `(?:abc)\d+`, true, true},
		{"non-capturing group", `(?:\d+)`, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Analyze(tc.pattern)
			if got.AtStart != tc.start {
				t.Errorf("AtStart: got %v, want %v (reason: %q)", got.AtStart, tc.start, got.Reason)
			}
			if got.AtEnd != tc.end {
				t.Errorf("AtEnd: got %v, want %v (reason: %q)", got.AtEnd, tc.end, got.Reason)
			}
		})
	}
}

func TestAnalyze_UnsafeStart(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
	}{
		// Already anchored
		{"caret anchor", `^foo`},
		{"word boundary", `\bfoo`},

		// Groups
		{"start-of-string alternation", `(?:^|foo)\d+`},
		{"capturing group", `(\d+)`},
		{"lookahead", `(?=\d)\d+`},
		{"negative lookahead", `(?!\d)\w+`},
		{"lookbehind", `(?<=\d)\w+`},

		// Non-word character constructs
		{"negated class", `[^a-z]\d+`},
		{"dot wildcard", `.+\d`},
		{"whitespace escape", `\s+\d+`},
		{"non-word escape", `\W+abc`},
		{"non-digit escape", `\D+`},

		// Char class without word chars
		{"class whitespace only", `[\s\t]+`},
		{"class dot space", `[ .]+`},
		// Mixed classes (word + non-word chars) are also unsafe
		{"mixed word and hyphen class", `[\w\-]+`},
		{"mixed digit and dot class", `[\d.]+`},

		// Bug 4: unicode category that is not a word
		{"separator unicode category", `\p{Z}+`},
		{"punctuation unicode category", `\p{P}+`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Analyze(tc.pattern)
			if got.AtStart {
				t.Errorf("expected AtStart=false for pattern %q, got true (reason: %q)", tc.pattern, got.Reason)
			}
		})
	}
}

func TestAnalyze_UnsafeEnd(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
	}{
		// Already anchored
		{"dollar anchor", `foo$`},
		{"word boundary suffix", `foo\b`},

		// Negated class at end
		{"negated class at end", `\d+[^a-z]`},
		{"negated class quantified", `\d+[^a-z]+`},

		// Non-word characters at end
		{"whitespace escape at end", `\d+\s`},
		{"non-word escape at end", `\d+\W`},
		{"dot at end", `\d+.`},
		{"dot quantified at end", `abc.+`},

		// Group with end-anchor alternation
		{"end-anchor alternation group", `\d+(?:foo|$)`},
		{"end-anchor in group", `abc(?:\s|$)`},

		// Literal non-word character at end
		{"hyphen literal", `\d+-`},
		{"space literal", `\d+ `},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Analyze(tc.pattern)
			if got.AtEnd {
				t.Errorf("expected AtEnd=false for pattern %q, got true (reason: %q)", tc.pattern, got.Reason)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// WrapIfSafe tests
// ─────────────────────────────────────────────────────────────────────────────

func TestWrapIfSafe(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		want    string
	}{
		// Both ends safe — wrap both.
		{"digit sequence", `\d{3}-\d{2}-\d{4}`, `\b\d{3}-\d{2}-\d{4}\b`},
		{"alpha class", `[A-Za-z]+`, `\b[A-Za-z]+\b`},
		{"word chars", `\w+`, `\b\w+\b`},

		// Only start safe — wrap start only (end is non-word).
		{"start safe end unsafe (dot)", `\d+.`, `\b\d+.`},
		{"start safe end unsafe (hyphen)", `\d+-`, `\b\d+-`},

		// Only end safe — wrap end only (start is non-word).
		{"end safe start unsafe (dot)", `.\d+`, `.\d+\b`},
		{"end safe start unsafe (negated)", `[^a-z]\d+`, `[^a-z]\d+\b`},

		// Neither end safe — no change.
		{"both unsafe", `[^a-z]\d+[^a-z]`, `[^a-z]\d+[^a-z]`},

		// UPI/email-style pattern with (?:^|…) and (?:…|$) anchoring.
		// Wrapping with \b breaks matching on "upi id: test@hdfc" — the analyzer
		// must detect the start/end alternation anchors and leave it unchanged.
		{"UPI pattern no wrap", `(?:^|[^a-zA-Z0-9.@])[a-zA-Z0-9.\-_]{2,256}@[a-zA-Z]{2,64}(?:[^a-zA-Z0-9.]|$)`,
			`(?:^|[^a-zA-Z0-9.@])[a-zA-Z0-9.\-_]{2,256}@[a-zA-Z]{2,64}(?:[^a-zA-Z0-9.]|$)`},

		// Already anchored — no double-wrapping.
		{"already starts with \\b", `\bfoo\b`, `\bfoo\b`},
		{"already starts with ^", `^foo$`, `^foo$`},

		// Leading flags: \b is inserted before the flag group because inserting
		// it afterwards would require rewriting the pattern string.
		{"(?i) prefix both ends safe", `(?i)\d+`, `\b(?i)\d+\b`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := WrapIfSafe(tc.pattern)
			if got != tc.want {
				t.Errorf("WrapIfSafe(%q)\n  got  %q\n  want %q", tc.pattern, got, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// stripLeadingFlags tests
// ─────────────────────────────────────────────────────────────────────────────

func TestStripLeadingFlags(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`(?i)\d+`, `\d+`},
		{`(?im)\w+`, `\w+`},
		{`(?imsx)abc`, `abc`},
		{`(?i)(?m)\d+`, `\d+`},
		{`\d+`, `\d+`},
		{`(?:foo)`, `(?:foo)`}, // non-capturing group — not a flag group
		{``, ``},
	}

	for _, tc := range cases {
		got := stripLeadingFlags(tc.in)
		if got != tc.want {
			t.Errorf("stripLeadingFlags(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// stripTrailingQuantifier tests
// ─────────────────────────────────────────────────────────────────────────────

func TestStripTrailingQuantifier(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`\d+`, `\d`},
		{`\d*`, `\d`},
		{`\d?`, `\d`},
		{`\d{4}`, `\d`},
		{`\d{2,4}`, `\d`},
		{`\d+?`, `\d`}, // lazy
		{`\w++`, `\w`}, // possessive
		{`[A-Z]+`, `[A-Z]`},
		{`abc`, `abc`}, // no quantifier
		{``, ``},
	}

	for _, tc := range cases {
		got := stripTrailingQuantifier(tc.in)
		if got != tc.want {
			t.Errorf("stripTrailingQuantifier(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// classContainsWordChar tests
// ─────────────────────────────────────────────────────────────────────────────

func TestClassContainsWordChar(t *testing.T) {
	cases := []struct {
		class string // full class including [ and ]
		want  bool
	}{
		{`[A-Za-z]`, true},
		{`[0-9]`, true},
		{`[\w]`, true},
		{`[\d]`, true},
		{`[A-Za-z0-9_]`, true},
		{`[\s\t]`, false},  // whitespace escapes → not a word class
		{`[ .-]`, false},   // space literal → not a word class
		{`[a-z\s]`, false}, // mixed: letters + whitespace → not a word class
		{`[\w\-]`, false},  // \- is escaped hyphen (non-word) → not a word class
		{`[\d.]`, false},   // dot literal → not a word class
		
		// Bug 1: escaped brackets
		{`[a-z\]]`, false},
		{`[\w\]]`, false},
	}

	for _, tc := range cases {
		got := classContainsWordChar(tc.class)
		if got != tc.want {
			t.Errorf("classContainsWordChar(%q) = %v, want %v", tc.class, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Real-world classification patterns
// ─────────────────────────────────────────────────────────────────────────────

func TestRealWorldPatterns(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		start   bool
		end     bool
	}{
		// SSN — digit at both ends
		{"SSN", `\d{3}[- ]?\d{2}[- ]?\d{4}`, true, true},

		// Credit card (Visa): starts with literal '4' (word char digit), ends with digit class
		{"Visa card", `4[0-9]{12}(?:[0-9]{3})?`, true, true},

		// Email: [A-Za-z0-9._%+\-] is a mixed class (dot/percent/plus/escaped-hyphen)
		// so the start is NOT safe; end [A-Za-z]{2,} is pure letters → safe.
		{"Email", `[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`, false, true},

		// IP address — digit at start, no word char... actually digit at end
		{"IP address", `\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`, true, true},

		// US phone — digit at both sides inside optional separators
		{"US phone parentheses", `\(?\d{3}\)?[- ]?\d{3}[- ]?\d{4}`, false, true},

		// UK NI: starts with pure letter class → safe; ends with [A-D\s] (mixed, has \s) → not safe
		{"UK NI", `[A-CEGHJ-PR-TW-Z]{1}[A-CEGHJ-NPR-TW-Z]{1}\d{6}[A-D\s]{1}`, true, false},

		// Pattern already wrapped — no re-wrap needed
		{"already wrapped", `\b\d{3}\b`, false, false},

		// Non-capturing group prefix — start unsafe
		{"NC group prefix", `(?:^|\s)\d{3}`, false, true},

		// UPI/email pattern with (?:^|…) start and (?:…|$) end — both unsafe.
		{"UPI ID", `(?:^|[^a-zA-Z0-9.@])[a-zA-Z0-9.\-_]{2,256}@[a-zA-Z]{2,64}(?:[^a-zA-Z0-9.]|$)`, false, false},

		// Unicode letter at start: Go's \b is ASCII-only, so adding \b before
		// a non-ASCII letter (like é) would break matching. Must be unsafe.
		{"unicode letter start", `éfoo`, false, true},
		{"unicode letter end", `foo_é`, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Analyze(tc.pattern)
			if got.AtStart != tc.start {
				t.Errorf("AtStart: got %v, want %v (reason: %q)", got.AtStart, tc.start, got.Reason)
			}
			if got.AtEnd != tc.end {
				t.Errorf("AtEnd: got %v, want %v (reason: %q)", got.AtEnd, tc.end, got.Reason)
			}
		})
	}
}
