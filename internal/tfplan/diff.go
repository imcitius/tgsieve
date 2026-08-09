package tfplan

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/imcitius/tgsieve/internal/model"
)

// flatten turns a nested JSON value into dotted attribute paths:
//
//	{"tags":{"a":"b"},"ports":[80,443]} ->
//	  tags.a=b, ports.0=80, ports.1=443
//
// Empty containers emit nothing; they are not changes on their own.
func flatten(v any, prefix string, out map[string]any) {
	switch t := v.(type) {
	case map[string]any:
		for k, sub := range t {
			flatten(sub, join(prefix, k), out)
		}
	case []any:
		for i, sub := range t {
			flatten(sub, join(prefix, strconv.Itoa(i)), out)
		}
	case nil:
		if prefix != "" {
			out[prefix] = nil
		}
	default:
		if prefix != "" {
			out[prefix] = t
		}
	}
}

func join(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// truePaths collects every path in a terraform "mask" structure
// (after_unknown, before_sensitive, after_sensitive) whose value is true.
// A true at a non-leaf position marks the whole subtree.
func truePaths(v any, prefix string, out map[string]bool) {
	switch t := v.(type) {
	case bool:
		if t {
			out[prefix] = true
		}
	case map[string]any:
		for k, sub := range t {
			truePaths(sub, join(prefix, k), out)
		}
	case []any:
		for i, sub := range t {
			truePaths(sub, join(prefix, strconv.Itoa(i)), out)
		}
	}
}

// markedAt reports whether path, or any of its ancestors, is in set.
func markedAt(set map[string]bool, path string) bool {
	if len(set) == 0 {
		return false
	}
	if set[""] { // whole value marked
		return true
	}
	if set[path] {
		return true
	}
	for i := 0; i < len(path); i++ {
		if path[i] == '.' && set[path[:i]] {
			return true
		}
	}
	return false
}

// Diff computes the attribute-level change list for one terraform change.
func Diff(ch Change) []model.AttrChange {
	before := map[string]any{}
	after := map[string]any{}
	flatten(ch.Before, "", before)
	flatten(ch.After, "", after)

	unknown := map[string]bool{}
	truePaths(ch.AfterUnknown, "", unknown)
	sensBefore := map[string]bool{}
	truePaths(ch.BeforeSensitive, "", sensBefore)
	sensAfter := map[string]bool{}
	truePaths(ch.AfterSensitive, "", sensAfter)

	keys := make(map[string]struct{}, len(before)+len(after))
	for k := range before {
		keys[k] = struct{}{}
	}
	for k := range after {
		keys[k] = struct{}{}
	}
	for k := range unknown {
		if k != "" {
			keys[k] = struct{}{}
		}
	}

	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Slice(sorted, func(i, j int) bool { return pathLess(sorted[i], sorted[j]) })

	out := make([]model.AttrChange, 0, len(sorted))
	for _, k := range sorted {
		bv, hasB := before[k]
		av, hasA := after[k]
		unk := markedAt(unknown, k)

		switch {
		case unk:
			// When a whole subtree is unknown, terraform itself reports the
			// outermost attribute as "(known after apply)". Emitting every
			// child underneath it would be the same fact repeated N times.
			if !unknown[k] && !unknown[""] {
				continue
			}
			if unknown[k] {
				bv = valueAt(ch.Before, k)
			}
		case !hasB && !hasA:
			continue
		case equalValue(bv, av):
			continue
		}

		out = append(out, model.AttrChange{
			Path:         k,
			Before:       bv,
			After:        av,
			AfterUnknown: unk,
			Sensitive:    markedAt(sensBefore, k) || markedAt(sensAfter, k),
		})
	}
	return out
}

// valueAt resolves a dotted path against the original nested value.
func valueAt(v any, path string) any {
	cur := v
	for _, seg := range strings.Split(path, ".") {
		switch t := cur.(type) {
		case map[string]any:
			sub, ok := t[seg]
			if !ok {
				return nil
			}
			cur = sub
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(t) {
				return nil
			}
			cur = t[i]
		default:
			return nil
		}
	}
	return cur
}

// pathLess sorts attribute paths so that numeric segments order naturally
// (foo.2 before foo.10) instead of lexically.
func pathLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
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

func equalValue(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	}
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}
