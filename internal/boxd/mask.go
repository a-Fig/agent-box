package boxd

import (
	"math"
	"regexp"
	"strings"
)

// Masking keeps tokens out of the history, which outlives the boxes
// (docs/ui.md, "Secrets in the history"). It is a best effort over commands,
// reasons and log tails before they are written.
var (
	// secretWord is box's secret-name rule (cmd/box/box.go's secretName).
	secretWord = `(?i:[A-Za-z0-9_-]*(?:TOKEN|SECRET|KEY|PASSWORD|PASSWD|CREDENTIAL)[A-Za-z0-9_-]*)`
	// NAME=value, NAME: value (JSON's "name": "value" too), and a flag
	// --name value.
	secretPair = regexp.MustCompile(`(` + secretWord + `)("?\s*=\s*|"?:\s*)("[^"]*"|'[^']*'|[^\s"',;]+)`)
	secretFlag = regexp.MustCompile(`(--?` + secretWord + `)(\s+)("[^"]*"|'[^']*'|[^\s"'-][^\s"']*)`)
	// Credentials in a URL: scheme://user:pass@host.
	urlCreds = regexp.MustCompile(`([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`)
	// Long runs of token characters (a JWT's parts are runs of their own).
	longRun = regexp.MustCompile(`[A-Za-z0-9+/=_-]{32,}`)
)

const masked = "***"

// Mask hides what looks like a secret in s.
func Mask(s string) string {
	s = urlCreds.ReplaceAllString(s, "${1}"+masked+"@")
	s = secretPair.ReplaceAllStringFunc(s, func(m string) string {
		sub := secretPair.FindStringSubmatch(m)
		if !strings.Contains(sub[2], "=") && len(strings.Trim(sub[3], `"'`)) < 8 {
			return m // "Key: run it" is prose; a value after a colon must look like one
		}
		return sub[1] + sub[2] + masked
	})
	s = secretFlag.ReplaceAllString(s, "${1}${2}"+masked)
	return longRun.ReplaceAllStringFunc(s, func(m string) string {
		if looksRandom(m) {
			return masked
		}
		return m
	})
}

// looksRandom says whether a long run is a token rather than a path, a
// package name or a digest: it mixes upper case, lower case and digits, and
// its entropy is above hex's 4 bits a character.
func looksRandom(s string) bool {
	return strings.ContainsAny(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") &&
		strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyz") &&
		strings.ContainsAny(s, "0123456789") && entropy(s) > 4
}

// entropy is the Shannon entropy of s in bits per character.
func entropy(s string) float64 {
	var n [256]int
	for i := 0; i < len(s); i++ {
		n[s[i]]++
	}
	h := 0.0
	for _, c := range n {
		if c > 0 {
			p := float64(c) / float64(len(s))
			h -= p * math.Log2(p)
		}
	}
	return h
}
