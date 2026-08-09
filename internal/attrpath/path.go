// Package attrpath builds and takes apart the dotted attribute paths used to
// name one field inside a resource.
package attrpath

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Attribute paths are dotted — tags.env, ports.0 — which is ambiguous the
// moment a map key contains a dot of its own. A key that is not a plain
// identifier is written in brackets instead:
//
//	tags["kubernetes.io/cluster"]
//	labels["app.kubernetes.io/name"]
//
// so a path can always be split back into the segments it was built from.

func plainKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// JoinKey appends a map key to a path.
func JoinKey(prefix, key string) string {
	if !plainKey(key) {
		quoted, err := json.Marshal(key)
		if err != nil {
			quoted = []byte(`"` + key + `"`)
		}
		return prefix + "[" + string(quoted) + "]"
	}
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// JoinIndex appends a list index to a path.
func JoinIndex(prefix string, i int) string {
	if prefix == "" {
		return strconv.Itoa(i)
	}
	return prefix + "." + strconv.Itoa(i)
}

// SplitPath returns the segments of a path, with bracketed keys unquoted.
func SplitPath(path string) []string {
	var segs []string
	var cur strings.Builder
	for i := 0; i < len(path); {
		switch path[i] {
		case '.':
			segs = append(segs, cur.String())
			cur.Reset()
			i++
		case '[':
			if cur.Len() > 0 {
				segs = append(segs, cur.String())
				cur.Reset()
			}
			end, key := readBracket(path, i)
			segs = append(segs, key)
			i = end
		default:
			cur.WriteByte(path[i])
			i++
		}
	}
	if cur.Len() > 0 {
		segs = append(segs, cur.String())
	}
	return segs
}

// readBracket reads a ["quoted"] segment starting at i, returning the index
// just past it and the unquoted key.
func readBracket(path string, i int) (int, string) {
	j := i + 1
	if j < len(path) && path[j] == '"' {
		k := j + 1
		for k < len(path) {
			if path[k] == '\\' {
				k += 2
				continue
			}
			if path[k] == '"' {
				break
			}
			k++
		}
		raw := path[j:min(k+1, len(path))]
		var key string
		if err := json.Unmarshal([]byte(raw), &key); err != nil {
			key = strings.Trim(raw, `"`)
		}
		if k+1 < len(path) && path[k+1] == ']' {
			return k + 2, key
		}
		return min(k+1, len(path)), key
	}
	// Unquoted bracket content: take it verbatim.
	end := strings.IndexByte(path[i:], ']')
	if end < 0 {
		return len(path), path[i+1:]
	}
	return i + end + 1, path[i+1 : i+end]
}

// Dotted rewrites bracketed segments as plain dotted ones, so a pattern
// written the ordinary way still matches a path that had to be quoted:
//
//	labels["app.kubernetes.io/name"] -> labels.app.kubernetes.io/name
func Dotted(path string) string {
	segs := SplitPath(path)
	if len(segs) == 0 {
		return path
	}
	return strings.Join(segs, ".")
}

// Ancestors lists every strict prefix of a path, outermost first.
func Ancestors(path string) []string {
	var out []string
	for i := 1; i < len(path); i++ {
		switch path[i] {
		case '.':
			out = append(out, path[:i])
		case '[':
			out = append(out, path[:i])
		}
	}
	return out
}

// PathLess orders attribute paths so numeric segments sort naturally
// (foo.2 before foo.10) instead of lexically.
func PathLess(a, b string) bool {
	as, bs := SplitPath(a), SplitPath(b)
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] == bs[i] {
			continue
		}
		an, aerr := strconv.Atoi(as[i])
		bn, berr := strconv.Atoi(bs[i])
		if aerr == nil && berr == nil {
			return an < bn
		}
		return as[i] < bs[i]
	}
	return len(as) < len(bs)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
