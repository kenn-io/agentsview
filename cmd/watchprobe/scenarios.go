package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// lifecycle uses explicit source notices; native delivery is measured separately.
func (p *probe) lifecycle(ctx context.Context, unit int, target string, checks map[string]bool) (total scanStats, err error) {
	before, err := freshSignature(target)
	if err != nil {
		return total, err
	}
	replacement := target + ".replacement"
	content := bytes.Repeat([]byte("R"), int(before.Size))
	if err = os.WriteFile(replacement, content, 0o600); err != nil {
		return total, err
	}
	stamp := time.Unix(0, before.MtimeNS)
	if err = os.Chtimes(replacement, stamp, stamp); err != nil {
		return total, err
	}
	if err = os.Rename(replacement, target); err != nil {
		return total, err
	}
	after, err := freshSignature(target)
	if err != nil {
		return total, err
	}
	s, err := p.check(ctx, unit, target)
	addStats(&total, s)
	if err != nil {
		return total, err
	}
	checks["replacement_identity"] = before.IdentityKnown && after.IdentityKnown && before.Identity != after.Identity && before.Size == after.Size && p.lastHash == sha256.Sum256(content)
	renamed := filepath.Join(filepath.Dir(target), "renamed.jsonl")
	if err = os.Rename(target, renamed); err != nil {
		return total, err
	}
	s, err = p.check(ctx, unit, target)
	addStats(&total, s)
	if err != nil {
		return total, err
	}
	s, err = p.check(ctx, unit, renamed)
	addStats(&total, s)
	if err != nil {
		return total, err
	}
	checks["rename_routes_new_name"] = s.Changed == 1 && p.lastHash == sha256.Sum256(content)
	if err = os.Remove(renamed); err != nil {
		return total, err
	}
	s, err = p.check(ctx, unit, renamed)
	addStats(&total, s)
	if err != nil {
		return total, err
	}
	_, missing := freshSignature(renamed)
	checks["remove_drops_cache_entry"] = errors.Is(missing, os.ErrNotExist)
	if p.cache != nil {
		known, e := p.cache.load(ctx, unit, []string{filepath.Base(target), filepath.Base(renamed)})
		if e != nil {
			return total, e
		}
		checks["remove_drops_cache_entry"] = checks["remove_drops_cache_entry"] && len(known) == 0
	}
	// Recreate the original workload path for sustained activity.
	if err = os.WriteFile(target, content, 0o600); err != nil {
		return total, err
	}
	s, err = p.check(ctx, unit, target)
	addStats(&total, s)
	checks["create_is_unknown"] = err == nil && s.Changed == 1
	return total, err
}
