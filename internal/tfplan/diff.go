package tfplan

import (
	"encoding/json"
	"sort"
	"strconv"

	"github.com/imcitius/tgsieve/internal/model"
)

// Diff computes the attribute-level change list for one terraform change.
//
// It walks the before and after values together rather than flattening both
// and subtracting, because the structure carries information that flattening
// throws away — most importantly for lists, where terraform renders sets as
// arrays and a reordering looks like every index changing at once.
func Diff(ch Change) []model.AttrChange {
	d := &differ{
		unknown: map[string]bool{},
		sensB:   map[string]bool{},
		sensA:   map[string]bool{},
	}
	truePaths(ch.AfterUnknown, "", d.unknown)
	truePaths(ch.BeforeSensitive, "", d.sensB)
	truePaths(ch.AfterSensitive, "", d.sensA)
	d.indexUnknownChildren()

	d.walk("", ch.Before, ch.After)
	sort.SliceStable(d.out, func(i, j int) bool { return PathLess(d.out[i].Path, d.out[j].Path) })
	return d.out
}

type differ struct {
	unknown map[string]bool
	sensB   map[string]bool
	sensA   map[string]bool
	// unknownKids lists attributes that exist only in the unknown mask —
	// computed values that appear in neither before nor after.
	unknownKids map[string][]string
	out         []model.AttrChange
}

func (d *differ) indexUnknownChildren() {
	d.unknownKids = map[string][]string{}
	for path := range d.unknown {
		if path == "" {
			continue
		}
		parent, key := parentOf(path)
		d.unknownKids[parent] = append(d.unknownKids[parent], key)
	}
	for k := range d.unknownKids {
		sort.Strings(d.unknownKids[k])
	}
}

// parentOf splits a path into its container and the final segment.
func parentOf(path string) (parent, key string) {
	ancestors := Ancestors(path)
	segs := SplitPath(path)
	if len(segs) == 0 {
		return "", path
	}
	key = segs[len(segs)-1]
	if len(ancestors) > 0 {
		parent = ancestors[len(ancestors)-1]
	}
	return parent, key
}

func (d *differ) walk(path string, before, after any) {
	if path != "" && d.unknownAt(path) {
		// terraform reports the outermost unknown attribute as
		// "(known after apply)"; repeating that for every child underneath
		// would be the same fact many times.
		if d.unknown[path] || !d.unknownAncestor(path) {
			d.emit(model.AttrChange{Path: path, Before: before, AfterUnknown: true})
		}
		return
	}

	switch b := before.(type) {
	case map[string]any:
		if a, ok := asMap(after); ok {
			d.walkMap(path, b, a)
			return
		}
	case []any:
		if a, ok := asSlice(after); ok {
			d.walkSlice(path, b, a)
			return
		}
	case nil:
		switch a := after.(type) {
		case map[string]any:
			d.walkMap(path, map[string]any{}, a)
			return
		case []any:
			d.walkSlice(path, nil, a)
			return
		}
	}

	if equalValue(before, after) {
		return
	}
	if path == "" {
		// A scalar at the root has no name to report it under.
		return
	}
	d.emit(model.AttrChange{Path: path, Before: before, After: after})
}

func (d *differ) walkMap(path string, before, after map[string]any) {
	keys := make([]string, 0, len(before)+len(after))
	seen := make(map[string]bool, len(before)+len(after))
	for k := range before {
		keys = append(keys, k)
		seen[k] = true
	}
	for k := range after {
		if !seen[k] {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	for _, k := range d.unknownKids[path] {
		if !seen[k] {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		d.walk(JoinKey(path, k), before[k], after[k])
	}
}

// walkSlice diffs two arrays. Terraform has no way to tell us whether an
// attribute is a set or a list, so the shape of the change decides how to
// report it: if the same elements come back in a different order, that is one
// fact, not N; if elements were added or removed, naming them beats walking
// positions that have all shifted by one.
func (d *differ) walkSlice(path string, before, after []any) {
	if equalValue(before, after) {
		return
	}
	if d.unknownUnder(path) || len(before) == 0 || len(after) == 0 {
		d.walkByIndex(path, before, after)
		return
	}

	if sameMultiset(before, after) {
		d.emit(model.AttrChange{Path: path, Kind: model.KindReordered, Count: len(before)})
		return
	}

	removed, added := multisetDiff(before, after)
	if !preferMembership(before, after, len(removed)+len(added)) {
		d.walkByIndex(path, before, after)
		return
	}
	for _, v := range removed {
		d.emit(model.AttrChange{Path: path, Before: v, Kind: model.KindRemoved})
	}
	for _, v := range added {
		d.emit(model.AttrChange{Path: path, After: v, Kind: model.KindAdded})
	}
}

func (d *differ) walkByIndex(path string, before, after []any) {
	n := len(before)
	if len(after) > n {
		n = len(after)
	}
	for i := 0; i < n; i++ {
		d.walk(JoinIndex(path, i), at(before, i), at(after, i))
	}
}

func (d *differ) emit(a model.AttrChange) {
	a.Sensitive = markedAt(d.sensB, a.Path) || markedAt(d.sensA, a.Path)
	d.out = append(d.out, a)
}

func (d *differ) unknownAt(path string) bool { return markedAt(d.unknown, path) }

func (d *differ) unknownAncestor(path string) bool {
	for _, p := range Ancestors(path) {
		if d.unknown[p] {
			return true
		}
	}
	return d.unknown[""]
}

// unknownUnder reports whether anything inside this path is unknown, in which
// case the array is walked by index so those entries can be reported where
// they belong.
func (d *differ) unknownUnder(path string) bool {
	prefix := path + "."
	for k := range d.unknown {
		if k == path || (len(k) > len(prefix) && k[:len(prefix)] == prefix) {
			return true
		}
	}
	return false
}

func at(s []any, i int) any {
	if i < len(s) {
		return s[i]
	}
	return nil
}

func asMap(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case nil:
		return map[string]any{}, true
	}
	return nil, false
}

func asSlice(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case nil:
		return nil, true
	}
	return nil, false
}

func key(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	return string(b)
}

func sameMultiset(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	counts := map[string]int{}
	for _, v := range a {
		counts[key(v)]++
	}
	for _, v := range b {
		k := key(v)
		counts[k]--
		if counts[k] < 0 {
			return false
		}
	}
	return true
}

// multisetDiff returns the elements only in before, and only in after,
// preserving their original order.
func multisetDiff(before, after []any) (removed, added []any) {
	inAfter := map[string]int{}
	for _, v := range after {
		inAfter[key(v)]++
	}
	inBefore := map[string]int{}
	for _, v := range before {
		inBefore[key(v)]++
	}
	for _, v := range before {
		k := key(v)
		if inAfter[k] > 0 {
			inAfter[k]--
			continue
		}
		removed = append(removed, v)
	}
	for _, v := range after {
		k := key(v)
		if inBefore[k] > 0 {
			inBefore[k]--
			continue
		}
		added = append(added, v)
	}
	return removed, added
}

// preferMembership decides between reporting an array by position and
// reporting what joined and left it.
//
// Positions are the clearer story only when they still line up: an element
// edited in place, or items appended to or trimmed from the end. Once the
// lengths differ in the middle, every later index has shifted and a positional
// report describes changes that did not happen — including a final
// "value → null" for an element that merely moved up.
func preferMembership(before, after []any, memberChanges int) bool {
	if len(before) != len(after) {
		return !isPrefix(before, after) && !isPrefix(after, before)
	}
	return memberChanges < positionalDiffs(before, after)
}

// isPrefix reports whether the shorter array is the start of the longer one.
func isPrefix(short, long []any) bool {
	if len(short) > len(long) {
		return false
	}
	for i := range short {
		if !equalValue(short[i], long[i]) {
			return false
		}
	}
	return true
}

// positionalDiffs counts the indices that do not match, which is roughly how
// many lines an index-by-index report would produce.
func positionalDiffs(before, after []any) int {
	n := len(before)
	if len(after) > n {
		n = len(after)
	}
	count := 0
	for i := 0; i < n; i++ {
		if !equalValue(at(before, i), at(after, i)) {
			count++
		}
	}
	return count
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
			truePaths(sub, JoinKey(prefix, k), out)
		}
	case []any:
		for i, sub := range t {
			truePaths(sub, JoinIndex(prefix, i), out)
		}
	}
}

// markedAt reports whether path, or any of its ancestors, is in set.
func markedAt(set map[string]bool, path string) bool {
	if len(set) == 0 {
		return false
	}
	if set[""] || set[path] {
		return true
	}
	for _, p := range Ancestors(path) {
		if set[p] {
			return true
		}
	}
	return false
}

// valueAt resolves a dotted path against the original nested value.
func valueAt(v any, path string) any {
	cur := v
	for _, seg := range SplitPath(path) {
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
