package runner

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// A module source pinned to a branch — or to a tag someone can move — reads
// identically before and after the code it names changes. Resolving the ref to
// a commit is the only way provenance can tell those apart.
//
// The resolution is one `git ls-remote` per distinct repository and ref, not
// per unit: a stack of a hundred units built from three modules costs three
// calls.

var (
	// git::https://host/org/repo.git//module?ref=v1  → the part before "//module"
	schemePrefixRe = regexp.MustCompile(`^[a-z0-9+.-]*::`)
	refParamRe     = regexp.MustCompile(`[?&]ref=([^&]+)`)
)

type refResolver struct {
	mu    sync.Mutex
	cache map[string]string
	off   bool
}

func newRefResolver(disabled bool) *refResolver {
	return &refResolver{cache: map[string]string{}, off: disabled}
}

// Resolve turns a module source into a value that changes when the code it
// points at changes. Local paths and unresolvable sources are returned
// unchanged: an unresolved ref is no worse than what was recorded before.
func (r *refResolver) Resolve(ctx context.Context, source string) string {
	if r.off {
		return source
	}
	repo, ref, ok := splitSource(source)
	if !ok {
		return source
	}
	key := repo + "\x00" + ref
	r.mu.Lock()
	sha, cached := r.cache[key]
	r.mu.Unlock()
	if !cached {
		sha = lsRemote(ctx, repo, ref)
		r.mu.Lock()
		r.cache[key] = sha
		r.mu.Unlock()
	}
	if sha == "" {
		return source
	}
	return source + "#" + sha
}

// splitSource pulls the repository and ref out of a terragrunt module source.
// Sources without a ref, and plain filesystem paths, are not git references.
func splitSource(source string) (repo, ref string, ok bool) {
	m := refParamRe.FindStringSubmatch(source)
	if m == nil {
		return "", "", false
	}
	ref = m[1]

	repo = source
	if i := strings.Index(repo, "?"); i >= 0 {
		repo = repo[:i]
	}
	repo = schemePrefixRe.ReplaceAllString(repo, "")
	// The module subdirectory is separated by "//", which also appears in
	// "https://" — so look past the scheme.
	search := repo
	offset := 0
	if i := strings.Index(repo, "://"); i >= 0 {
		offset = i + 3
		search = repo[offset:]
	}
	if i := strings.Index(search, "//"); i >= 0 {
		repo = repo[:offset+i]
	}
	repo = strings.TrimSuffix(repo, "/")
	if repo == "" || strings.HasPrefix(repo, ".") || strings.HasPrefix(repo, "/") {
		return "", "", false
	}
	return repo, ref, true
}

// envWithout returns the environment with one variable removed, so a caller's
// setting cannot re-enable an interactive prompt.
func envWithout(key string) []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		if strings.HasPrefix(kv, key+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func lsRemote(ctx context.Context, repo, ref string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--quiet", repo, ref)
	// Never let git stop for credentials: this is a best-effort enrichment,
	// and a hung prompt would be worse than an unresolved ref.
	cmd.Env = append(envWithout("GIT_TERMINAL_PROMPT"), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			return fields[0]
		}
	}
	return ""
}
