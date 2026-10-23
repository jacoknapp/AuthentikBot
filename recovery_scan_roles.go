package main

import (
	"context"
	"sync"
)

type recoveryScanRoleKey struct {
	ID             string
	FullVisibility bool
}
type recoveryScanRoleResult struct {
	Done    chan struct{}
	Entries []string
	Err     error
}
type recoveryScanRoles struct {
	mu      sync.Mutex
	entries map[recoveryScanRoleKey]*recoveryScanRoleResult
}

func newRecoveryScanRoles() *recoveryScanRoles {
	return &recoveryScanRoles{entries: map[recoveryScanRoleKey]*recoveryScanRoleResult{}}
}

// This memo belongs to one bounded background suggestion scan, never the main
// recovery API. Shared-role grant reads are single-flight by UUID, not role name.
// Errors are memoized too: concurrent callers do not retry a denied/failed read.
// User records, credential identity and effective role membership stay uncached.
// A subsequent scan gets a new memo. Actual preview/confirmation APIs have none.
func (a *recoveryAPI) rolePermissionEntries(id string, fullVisibility bool) ([]string, error) {
	if !validRecoveryUUID(id) {
		return nil, errRecoveryProtected
	}
	if a.scanRoles == nil {
		return a.loadRolePermissionEntries(id, fullVisibility)
	}
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return nil, errRecoveryAPI
	}
	key := recoveryScanRoleKey{ID: id, FullVisibility: fullVisibility}
	memo := a.scanRoles
	memo.mu.Lock()
	entry, found := memo.entries[key]
	if !found {
		entry = &recoveryScanRoleResult{Done: make(chan struct{})}
		memo.entries[key] = entry
	}
	memo.mu.Unlock()
	if found {
		select {
		case <-entry.Done:
		case <-ctx.Done():
			return nil, errRecoveryAPI
		}
	} else {
		entry.Entries, entry.Err = a.loadRolePermissionEntries(id, fullVisibility)
		close(entry.Done)
	}
	if ctx.Err() != nil {
		return nil, errRecoveryAPI
	}
	return append([]string{}, entry.Entries...), entry.Err
}
