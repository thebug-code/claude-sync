package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// sessionFileExt is the extension of a session transcript under projects/.
const sessionFileExt = ".jsonl"

// maxAmbiguousListed caps how many candidates an ambiguous-prefix error lists.
const maxAmbiguousListed = 10

// ErrSessionNotFound is returned when no local session matches a query.
var ErrSessionNotFound = errors.New("session not found")

// ErrAmbiguousSession is returned when a prefix matches more than one session.
var ErrAmbiguousSession = errors.New("ambiguous session id")

// SessionRef identifies one Claude Code session on disk. A session is the
// transcript projects/<project>/<id>.jsonl, plus its sibling directory
// projects/<project>/<id>/ (subagents, tool results) and tasks/<id>/ when
// those exist.
type SessionRef struct {
	ID       string   // full session ID (transcript file stem)
	Projects []string // encoded project directory names under projects/
	Paths    []string // claudeDir-relative, slash-separated files or directories
}

// ResolveSession finds the local session matching query, which may be a full
// session ID or a unique prefix of one (a trailing ".jsonl" is ignored). An
// exact ID match wins over longer IDs sharing it as a prefix.
func ResolveSession(claudeDir, query string) (*SessionRef, error) {
	query = strings.TrimSuffix(strings.TrimSpace(query), sessionFileExt)
	if query == "" || query == "." || query == ".." || strings.ContainsAny(query, `/\`) {
		return nil, fmt.Errorf("invalid session id %q", query)
	}

	projectsDir := filepath.Join(claudeDir, "projects")
	projects, err := os.ReadDir(projectsDir)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: %s (no projects directory)", ErrSessionNotFound, query)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read projects directory: %w", err)
	}

	// id -> projects containing a transcript for it
	matches := make(map[string][]string)
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(projectsDir, project.Name()))
		if err != nil {
			return nil, fmt.Errorf("failed to read project %s: %w", project.Name(), err)
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), sessionFileExt) {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), sessionFileExt)
			if strings.HasPrefix(id, query) {
				matches[id] = append(matches[id], project.Name())
			}
		}
	}

	var id string
	switch {
	case len(matches[query]) > 0:
		id = query
	case len(matches) == 0:
		return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, query)
	case len(matches) == 1:
		for only := range matches {
			id = only
		}
	default:
		ids := make([]string, 0, len(matches))
		for m := range matches {
			ids = append(ids, m)
		}
		sort.Strings(ids)
		listed := ids
		if len(listed) > maxAmbiguousListed {
			listed = listed[:maxAmbiguousListed]
		}
		msg := strings.Join(listed, ", ")
		if len(ids) > len(listed) {
			msg += fmt.Sprintf(", … (%d more)", len(ids)-len(listed))
		}
		return nil, fmt.Errorf("%w: %q matches %d sessions: %s", ErrAmbiguousSession, query, len(ids), msg)
	}

	ref := &SessionRef{ID: id, Projects: matches[id]}
	sort.Strings(ref.Projects)
	for _, project := range ref.Projects {
		ref.Paths = append(ref.Paths, path.Join("projects", project, id+sessionFileExt))
		if isDir(filepath.Join(projectsDir, project, id)) {
			ref.Paths = append(ref.Paths, path.Join("projects", project, id))
		}
	}
	if isDir(filepath.Join(claudeDir, "tasks", id)) {
		ref.Paths = append(ref.Paths, path.Join("tasks", id))
	}
	return ref, nil
}

func isDir(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.IsDir()
}

// coveredBy reports whether relPath is one of roots or lives under one of them.
func coveredBy(relPath string, roots []string) bool {
	for _, root := range roots {
		root = strings.TrimSuffix(filepath.ToSlash(root), "/")
		if relPath == root || strings.HasPrefix(relPath, root+"/") {
			return true
		}
	}
	return false
}

// ResolveSessions resolves each query to a session and restricts its paths to
// what this syncer is configured to sync. A session whose transcript falls
// outside the effective sync paths, or is excluded, is an error; auxiliary
// paths (such as tasks/<id>) outside them are silently dropped. Queries that
// resolve to the same session are deduplicated.
func (s *Syncer) ResolveSessions(queries []string) ([]*SessionRef, error) {
	syncPaths := s.syncPaths()
	seen := make(map[string]bool)
	var refs []*SessionRef

	for _, q := range queries {
		ref, err := ResolveSession(s.claudeDir, q)
		if err != nil {
			return nil, err
		}
		if seen[ref.ID] {
			continue
		}
		seen[ref.ID] = true

		var kept []string
		for _, p := range ref.Paths {
			isTranscript := strings.HasSuffix(p, sessionFileExt)
			if !coveredBy(p, syncPaths) {
				if isTranscript {
					return nil, fmt.Errorf("session %s (%s) is outside the configured sync paths", ref.ID, p)
				}
				continue
			}
			if isTranscript && s.isExcluded(p) {
				return nil, fmt.Errorf("session %s (%s) is excluded by config", ref.ID, p)
			}
			kept = append(kept, p)
		}
		ref.Paths = kept
		refs = append(refs, ref)
	}
	return refs, nil
}

// PushSessions uploads only the files belonging to the given sessions. Unlike
// Push, deletions are limited to files that were previously synced from within
// those sessions' own paths, so nothing else on the remote is touched. The
// global LastPush/LastSync timestamps are left alone because the rest of
// ~/.claude was not synced; per-file state is still updated.
func (s *Syncer) PushSessions(ctx context.Context, refs []*SessionRef) (*SyncResult, error) {
	var paths []string
	for _, ref := range refs {
		paths = append(paths, ref.Paths...)
	}
	if len(paths) == 0 {
		return &SyncResult{}, nil
	}

	s.progress(ProgressEvent{Action: "scan", Path: "Detecting session changes..."})

	changes, err := s.state.DetectChangesWithin(s.claudeDir, paths, s.isExcluded)
	if err != nil {
		return nil, fmt.Errorf("failed to detect changes: %w", err)
	}
	return s.pushChanges(ctx, changes, false)
}
