package runner

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"sync"
)

// renderedConfig is the part of `terragrunt render --format json` we care
// about: where a unit's terraform code actually comes from.
type renderedConfig struct {
	Terraform struct {
		Source string `json:"source"`
	} `json:"terraform"`
}

// ModuleSources resolves the module source of each unit.
//
// Provenance covers the configuration in the working directory, but a unit
// whose source points at a remote module can change underneath an unchanged
// repository. Reading the resolved source catches a moved ref.
//
// `render --all` writes its objects to stdout unlabelled, so units cannot be
// told apart in that stream; each unit is rendered on its own instead, in
// parallel. Units that fail to render are simply absent from the result —
// missing information must not masquerade as "unchanged".
func ModuleSources(ctx context.Context, opts Options, units []string) map[string]string {
	if len(units) == 0 {
		return nil
	}
	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	if workers > len(units) {
		workers = len(units)
	}

	jobs := make(chan string)
	var mu sync.Mutex
	out := make(map[string]string, len(units))

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for unit := range jobs {
				unitOpts := opts
				unitOpts.Dir = filepath.Join(opts.Dir, filepath.FromSlash(unit))
				raw, err := output(ctx, unitOpts, []string{"render", "--format", "json"})
				if err != nil {
					continue
				}
				var cfg renderedConfig
				if err := json.Unmarshal(raw, &cfg); err != nil {
					continue
				}
				if cfg.Terraform.Source == "" {
					continue
				}
				mu.Lock()
				out[unit] = cfg.Terraform.Source
				mu.Unlock()
			}
		}()
	}
	for _, u := range units {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return out
		case jobs <- u:
		}
	}
	close(jobs)
	wg.Wait()
	return out
}
