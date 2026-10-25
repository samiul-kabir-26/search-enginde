package tenant

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"github.com/redis/go-redis/v9"
)

// ChannelConfigActivated carries config activation and shadow changes from control-api to
// every search-api replica (docs/ARCHITECTURE.md §8).
const ChannelConfigActivated = "tenant.config.activated"

// Activation is the message on ChannelConfigActivated.
type Activation struct {
	TenantID string `json:"tenant_id"`
	Version  int    `json:"version"`
	// State is "active", "shadow" or "shadow_cleared".
	State string `json:"state"`
}

// PublishActivation announces a change. Pub/sub is lossy; search-api's reconcile loop is the
// backstop.
func PublishActivation(ctx context.Context, rdb *redis.Client, a Activation) error {
	b, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("activation: %w", err)
	}
	if err := rdb.Publish(ctx, ChannelConfigActivated, b).Err(); err != nil {
		return fmt.Errorf("publish activation: %w", err)
	}
	return nil
}

// Change is one difference between two config documents.
type Change struct {
	Path string `json:"path"`
	Op   string `json:"op"`
	From any    `json:"from,omitempty"`
	To   any    `json:"to,omitempty"`
}

// Diff compares two documents and returns the changed paths (JSON Pointers) in order. Objects
// are compared key by key; arrays and scalars are compared whole.
func Diff(a, b json.RawMessage) ([]Change, error) {
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		return nil, fmt.Errorf("diff from: %w", err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		return nil, fmt.Errorf("diff to: %w", err)
	}
	out := []Change{}
	diff("", x, y, &out)
	return out, nil
}

func diff(path string, x, y any, out *[]Change) {
	mx, okx := x.(map[string]any)
	my, oky := y.(map[string]any)
	if okx && oky {
		keys := map[string]bool{}
		for k := range mx {
			keys[k] = true
		}
		for k := range my {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			p := path + "/" + k
			vx, inx := mx[k]
			vy, iny := my[k]
			switch {
			case !inx:
				*out = append(*out, Change{Path: p, Op: "add", To: vy})
			case !iny:
				*out = append(*out, Change{Path: p, Op: "remove", From: vx})
			default:
				diff(p, vx, vy, out)
			}
		}
		return
	}
	if !reflect.DeepEqual(x, y) {
		*out = append(*out, Change{Path: path, Op: "replace", From: x, To: y})
	}
}
