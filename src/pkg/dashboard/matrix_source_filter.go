package dashboard

import (
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hivecommons/hive/crux/logscrub"
)

// matrixEntropyRun is the longest run of token-alphabet characters a Matrix
// line may carry; a longer run looks like a key, hash or encoded secret.
const matrixEntropyRun = 32

// matrixKeyValueSecret matches a secret-looking key, optionally closed by a
// quote as in JSON, PHP or Ruby (`"password": "x"`, `'password' => 'x'`),
// followed by `=`, `:` or `=>` and a value, so short secrets such as
// `password: hunter2` are dropped. Go's `:=` and `==` are excluded so ordinary
// code is not over-dropped.
var matrixKeyValueSecret = regexp.MustCompile(`(?i)(?:password|passwd|pwd|secret|api[_-]?key|access[_-]?key|auth[_-]?token|token)["']?\s*(?:=[^=]|:[^=])\s*["']?[^\s"']`)

var matrixLockfiles = map[string]bool{
	"go.sum":              true,
	"package-lock.json":   true,
	"npm-shrinkwrap.json": true,
	"yarn.lock":           true,
	"pnpm-lock.yaml":      true,
	"cargo.lock":          true,
	"gemfile.lock":        true,
	"poetry.lock":         true,
	"pipfile.lock":        true,
	"composer.lock":       true,
	"bun.lockb":           true,
}

// matrixPathDenied reports whether a file must be skipped before its patch is
// parsed: secret-shaped names, lockfiles, vendored or generated code, and the
// operator's dashboard.matrix.exclude_paths globs.
func matrixPathDenied(p string, excludes []string) bool {
	if strings.TrimSpace(p) == "" {
		return true
	}
	lower := strings.ToLower(p)
	base := path.Base(lower)
	switch {
	case strings.HasPrefix(base, ".env"),
		strings.HasSuffix(base, ".pem"),
		strings.HasSuffix(base, ".key"),
		strings.HasSuffix(base, ".p12"),
		strings.HasPrefix(base, "id_"),
		matrixLockfiles[base],
		strings.HasSuffix(base, ".lock"),
		strings.Contains(base, ".min."),
		strings.Contains(lower, "zz_generated"),
		strings.HasSuffix(base, ".pb.go"),
		strings.Contains(base, "_gen."):
		return true
	}
	segments := strings.Split(lower, "/")
	for i, seg := range segments {
		if strings.Contains(seg, "secret") || strings.Contains(seg, "credential") || strings.Contains(seg, "token") {
			return true
		}
		if i < len(segments)-1 && (seg == "vendor" || seg == "node_modules") {
			return true
		}
	}
	for _, glob := range excludes {
		glob = strings.TrimSpace(glob)
		if glob == "" {
			continue
		}
		if ok, _ := path.Match(glob, p); ok {
			return true
		}
		if ok, _ := path.Match(glob, path.Base(p)); ok {
			return true
		}
		if dir := strings.TrimSuffix(glob, "/**"); dir != glob && (p == dir || strings.HasPrefix(p, dir+"/")) {
			return true
		}
	}
	return false
}

// matrixPatchLine is a changed line, retaining indentation and blank lines.
type matrixPatchLine struct {
	change string
	text   string
}

// matrixPatchBlock is one contiguous changed region of a unified diff.
// Context and hunk boundaries separate blocks; neither is displayed as code.
type matrixPatchBlock struct {
	lines []matrixPatchLine
	raw   string
	bytes int
}

// matrixDiffBlocks admits complete changed regions, including deletions.
// A rejected line rejects its whole region, so filtering cannot splice two
// unrelated pieces into what appears to be a complete paragraph. Armour is
// tracked through context as well as both changed sides of the diff.
func matrixDiffBlocks(patch string) []matrixPatchBlock {
	if patch == "" || strings.ContainsRune(patch, 0) {
		return nil
	}
	var out []matrixPatchBlock
	var lines []matrixPatchLine
	inHunk, inArmour, rejected, content := false, false, false, false
	start, end, offset, size := 0, 0, 0, 0
	flush := func() {
		if !rejected && content && len(lines) > 0 {
			out = append(out, matrixPatchBlock{lines: lines, raw: patch[start:end], bytes: size})
			lines = nil
		} else {
			clear(lines)
			lines = lines[:0]
		}
		rejected, content, size = false, false, 0
	}
	for raw := range strings.SplitSeq(patch, "\n") {
		lineStart := offset
		offset = min(len(patch), offset+len(raw)+1)
		if strings.HasPrefix(raw, "@@ ") || raw == "@@" {
			flush()
			inHunk = true
			continue
		}
		if !inHunk || strings.HasPrefix(raw, "\\") {
			continue
		}
		if raw == "" {
			flush()
			continue
		}
		begin := strings.Contains(raw[1:], "-----BEGIN ")
		finish := strings.Contains(raw[1:], "-----END ")
		armoured := inArmour || begin || finish
		if begin {
			inArmour = true
		}
		if finish {
			inArmour = false
		}
		if raw[0] != '+' && raw[0] != '-' {
			flush()
			continue
		}
		if len(lines) == 0 {
			start = lineStart
		}
		end = offset
		if armoured {
			rejected = true
		}
		line, ok := matrixAcceptLine(raw[1:])
		size += len(line) + 1
		if !ok || len(lines) >= matrixMaxLinesPerHead || size > matrixMaxBlockBytes {
			rejected = true
		}
		if rejected {
			continue
		}
		change := "add"
		if raw[0] == '-' {
			change = "remove"
		}
		lines = append(lines, matrixPatchLine{change: change, text: line})
		content = content || strings.TrimSpace(line) != ""
	}
	flush()
	return out
}

// matrixAcceptLine preserves whitespace and rejects unsafe or oversized
// content without masking or truncating it. Blank lines are retained only
// within a block containing nonblank changed text.
func matrixAcceptLine(raw string) (string, bool) {
	line := strings.TrimSuffix(raw, "\r")
	if !utf8.ValidString(line) || utf8.RuneCountInString(line) > matrixMaxLineRunes {
		return "", false
	}
	for _, r := range line {
		if unicode.IsControl(r) && r != '\t' {
			return "", false
		}
	}
	if matrixHasEntropyRun(line) {
		return "", false
	}
	if matrixKeyValueSecret.MatchString(line) {
		return "", false
	}
	if logscrub.ContainsRedactionMarker(line) {
		return "", false
	}
	if logscrub.ScrubString(line) != line || logscrub.RedactPaneTokens(line) != line {
		return "", false
	}
	return line, true
}

func matrixHasEntropyRun(s string) bool {
	run := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '+' || c == '/' || c == '=' || c == '_' || c == '-' {
			run++
			if run >= matrixEntropyRun {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}

var matrixLangByExt = map[string]string{
	".go":   "go",
	".js":   "javascript",
	".mjs":  "javascript",
	".cjs":  "javascript",
	".jsx":  "javascript",
	".ts":   "typescript",
	".tsx":  "typescript",
	".py":   "python",
	".rb":   "ruby",
	".rs":   "rust",
	".java": "java",
	".kt":   "kotlin",
	".c":    "c",
	".h":    "c",
	".cc":   "cpp",
	".cpp":  "cpp",
	".hpp":  "cpp",
	".cs":   "csharp",
	".sh":   "shell",
	".bash": "shell",
	".yaml": "yaml",
	".yml":  "yaml",
	".json": "json",
	".toml": "toml",
	".md":   "markdown",
	".html": "html",
	".css":  "css",
	".sql":  "sql",
}

// matrixLang names a file's language from its extension; "" when unknown.
func matrixLang(p string) string {
	return matrixLangByExt[strings.ToLower(path.Ext(p))]
}
