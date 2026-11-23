package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	sessA = "5d67ed06-0c52-4002-a73a-574a16937476"
	sessB = "5d67ffff-df09-48e8-9871-b808c616c67e"
	sessC = "95328183-a8f2-4919-bf7b-c44b41a0b723"
)

// writeSessionTree lays out two projects with three sessions; sessA has a
// tool-results directory and a tasks directory.
func writeSessionTree(t *testing.T, claudeDir string) {
	t.Helper()
	writeFile(t, claudeDir, "projects/-home-u-foo/"+sessA+".jsonl", `{"a":1}`)
	writeFile(t, claudeDir, "projects/-home-u-foo/"+sessA+"/tool-results/r1.txt", "result 1")
	writeFile(t, claudeDir, "projects/-home-u-foo/"+sessB+".jsonl", `{"b":1}`)
	writeFile(t, claudeDir, "projects/-home-u-bar/"+sessC+".jsonl", `{"c":1}`)
	writeFile(t, claudeDir, "projects/-home-u-bar/memory/MEMORY.md", "memory")
	writeFile(t, claudeDir, "tasks/"+sessA+"/1.json", `{"task":1}`)
	writeFile(t, claudeDir, "CLAUDE.md", "# global")
	writeFile(t, claudeDir, "history.jsonl", `{"display":"hi"}`)
}

func remoteKeys(t *testing.T, env *testEnv) []string {
	t.Helper()
	objs, err := env.store.ListUserObjects(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	keys := make([]string, 0, len(objs))
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	sort.Strings(keys)
	return keys
}

func TestResolveSession_ExactAndPrefix(t *testing.T) {
	env := setupTestEnv(t)
	writeSessionTree(t, env.claudeDir)

	for _, q := range []string{sessA, "5d67ed", sessA + ".jsonl", "  5d67ed06  "} {
		ref, err := ResolveSession(env.claudeDir, q)
		if err != nil {
			t.Fatalf("ResolveSession(%q): %v", q, err)
		}
		if ref.ID != sessA {
			t.Errorf("ResolveSession(%q).ID = %s, want %s", q, ref.ID, sessA)
		}
		want := []string{
			"projects/-home-u-foo/" + sessA + ".jsonl",
			"projects/-home-u-foo/" + sessA,
			"tasks/" + sessA,
		}
		if strings.Join(ref.Paths, "|") != strings.Join(want, "|") {
			t.Errorf("Paths = %v, want %v", ref.Paths, want)
		}
		if len(ref.Projects) != 1 || ref.Projects[0] != "-home-u-foo" {
			t.Errorf("Projects = %v", ref.Projects)
		}
	}

	ref, err := ResolveSession(env.claudeDir, "9532")
	if err != nil {
		t.Fatalf("resolve sessC: %v", err)
	}
	if ref.ID != sessC || len(ref.Paths) != 1 {
		t.Errorf("sessC ref = %+v", ref)
	}
}

func TestResolveSession_ExactWinsOverLongerPrefixMatch(t *testing.T) {
	env := setupTestEnv(t)
	writeFile(t, env.claudeDir, "projects/p/abc.jsonl", "{}")
	writeFile(t, env.claudeDir, "projects/p/abcdef.jsonl", "{}")

	ref, err := ResolveSession(env.claudeDir, "abc")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ref.ID != "abc" {
		t.Errorf("ID = %s, want abc", ref.ID)
	}
}

func TestResolveSession_Errors(t *testing.T) {
	env := setupTestEnv(t)

	if _, err := ResolveSession(env.claudeDir, sessA); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("missing projects dir: got %v, want ErrSessionNotFound", err)
	}

	writeSessionTree(t, env.claudeDir)

	_, err := ResolveSession(env.claudeDir, "5d67")
	if !errors.Is(err, ErrAmbiguousSession) {
		t.Fatalf("ambiguous: got %v", err)
	}
	if !strings.Contains(err.Error(), sessA) || !strings.Contains(err.Error(), sessB) {
		t.Errorf("ambiguous error should list candidates: %v", err)
	}

	if _, err := ResolveSession(env.claudeDir, "nope"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("not found: got %v", err)
	}
	// "memory" is a directory, not a session transcript
	if _, err := ResolveSession(env.claudeDir, "memory"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("memory dir: got %v", err)
	}

	for _, q := range []string{"", "  ", ".", "..", "../x", `a\b`, "-home-u-foo/" + sessA} {
		if _, err := ResolveSession(env.claudeDir, q); err == nil || errors.Is(err, ErrSessionNotFound) {
			t.Errorf("ResolveSession(%q) = %v, want invalid id error", q, err)
		}
	}
}

func TestResolveSession_AmbiguousListIsCapped(t *testing.T) {
	env := setupTestEnv(t)
	for i := 0; i < maxAmbiguousListed+3; i++ {
		writeFile(t, env.claudeDir, "projects/p/x"+string(rune('a'+i))+".jsonl", "{}")
	}
	_, err := ResolveSession(env.claudeDir, "x")
	if !errors.Is(err, ErrAmbiguousSession) || !strings.Contains(err.Error(), "(3 more)") {
		t.Errorf("got %v", err)
	}
}

func TestResolveSession_MultipleProjects(t *testing.T) {
	env := setupTestEnv(t)
	writeFile(t, env.claudeDir, "projects/p2/"+sessC+".jsonl", "{}")
	writeFile(t, env.claudeDir, "projects/p1/"+sessC+".jsonl", "{}")

	ref, err := ResolveSession(env.claudeDir, sessC)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if strings.Join(ref.Projects, ",") != "p1,p2" || len(ref.Paths) != 2 {
		t.Errorf("ref = %+v", ref)
	}
}

func TestResolveSessions_ScopeAndExcludes(t *testing.T) {
	env := setupTestEnv(t)
	writeSessionTree(t, env.claudeDir)

	// Dedup: two queries for the same session
	refs, err := env.syncer.ResolveSessions([]string{sessA, "5d67ed"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(refs) != 1 || len(refs[0].Paths) != 3 {
		t.Fatalf("refs = %+v", refs)
	}

	// tasks/ outside the sync paths is dropped, transcript is kept
	env.syncer.cfg.SyncPaths = []string{"projects"}
	refs, err = env.syncer.ResolveSessions([]string{sessA})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, p := range refs[0].Paths {
		if strings.HasPrefix(p, "tasks/") {
			t.Errorf("tasks path should be dropped: %v", refs[0].Paths)
		}
	}

	// projects/ outside the sync paths is an error
	env.syncer.cfg.SyncPaths = []string{"CLAUDE.md"}
	if _, err := env.syncer.ResolveSessions([]string{sessA}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("want outside-sync-paths error, got %v", err)
	}

	// excluded transcript is an error
	env.syncer.cfg.SyncPaths = nil
	env.syncer.cfg.Exclude = []string{"projects/-home-u-foo"}
	if _, err := env.syncer.ResolveSessions([]string{sessA}); err == nil || !strings.Contains(err.Error(), "excluded") {
		t.Errorf("want excluded error, got %v", err)
	}

	// resolution errors propagate
	env.syncer.cfg.Exclude = nil
	if _, err := env.syncer.ResolveSessions([]string{"nope"}); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("got %v", err)
	}
}

func TestPushSessions_UploadsOnlySessionFiles(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	writeSessionTree(t, env.claudeDir)

	refs, err := env.syncer.ResolveSessions([]string{"5d67ed"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	result, err := env.syncer.PushSessions(ctx, refs)
	if err != nil {
		t.Fatalf("PushSessions: %v", err)
	}
	if len(result.Errors) > 0 {
		t.Fatalf("errors: %v", result.Errors)
	}

	want := []string{
		"projects/-home-u-foo/" + sessA + "/tool-results/r1.txt.age",
		"projects/-home-u-foo/" + sessA + ".jsonl.age",
		"tasks/" + sessA + "/1.json.age",
	}
	sort.Strings(want)
	if got := remoteKeys(t, env); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("remote keys = %v, want %v", got, want)
	}

	if !env.syncer.state.LastPush.IsZero() || !env.syncer.state.LastSync.IsZero() {
		t.Errorf("partial push must not set LastPush/LastSync")
	}
	// Per-file state is persisted, so a later full push skips these files.
	reloaded, err := LoadStateFromDir(env.stateDir)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if reloaded.GetFile("projects/-home-u-foo/"+sessA+".jsonl") == nil {
		t.Errorf("session file missing from saved state")
	}

	full, err := env.syncer.Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	for _, p := range full.Uploaded {
		if strings.Contains(p, sessA) {
			t.Errorf("full push re-uploaded session file %s", p)
		}
	}
	if env.syncer.state.LastPush.IsZero() {
		t.Errorf("full push should set LastPush")
	}
}

func TestPushSessions_NeverDeletesOutsideSession(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	writeSessionTree(t, env.claudeDir)

	if _, err := env.syncer.Push(ctx); err != nil {
		t.Fatalf("initial push: %v", err)
	}
	before := remoteKeys(t, env)
	lastPush := env.syncer.state.LastPush

	// Remove files belonging to other sessions and global config locally.
	for _, p := range []string{
		"projects/-home-u-foo/" + sessB + ".jsonl",
		"projects/-home-u-bar/" + sessC + ".jsonl",
		"CLAUDE.md",
		"history.jsonl",
	} {
		if err := os.Remove(filepath.Join(env.claudeDir, p)); err != nil {
			t.Fatal(err)
		}
	}
	// And modify the target session.
	time.Sleep(10 * time.Millisecond)
	writeFile(t, env.claudeDir, "projects/-home-u-foo/"+sessA+".jsonl", `{"a":2}`)

	refs, err := env.syncer.ResolveSessions([]string{sessA})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	result, err := env.syncer.PushSessions(ctx, refs)
	if err != nil {
		t.Fatalf("PushSessions: %v", err)
	}
	if len(result.Deleted) != 0 {
		t.Errorf("session push deleted %v", result.Deleted)
	}
	if len(result.Uploaded) != 1 || result.Uploaded[0] != "projects/-home-u-foo/"+sessA+".jsonl" {
		t.Errorf("uploaded = %v", result.Uploaded)
	}
	if after := remoteKeys(t, env); strings.Join(after, "|") != strings.Join(before, "|") {
		t.Errorf("remote keys changed:\nbefore %v\nafter  %v", before, after)
	}
	if !env.syncer.state.LastPush.Equal(lastPush) {
		t.Errorf("LastPush changed on partial push")
	}
}

func TestPushSessions_DeletesWithinSession(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	writeSessionTree(t, env.claudeDir)

	refs, err := env.syncer.ResolveSessions([]string{sessA})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := env.syncer.PushSessions(ctx, refs); err != nil {
		t.Fatalf("first push: %v", err)
	}

	toolResult := "projects/-home-u-foo/" + sessA + "/tool-results/r1.txt"
	if err := os.Remove(filepath.Join(env.claudeDir, toolResult)); err != nil {
		t.Fatal(err)
	}

	refs, err = env.syncer.ResolveSessions([]string{sessA})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	result, err := env.syncer.PushSessions(ctx, refs)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0] != toolResult {
		t.Errorf("deleted = %v, want [%s]", result.Deleted, toolResult)
	}
	for _, k := range remoteKeys(t, env) {
		if k == toolResult+".age" {
			t.Errorf("tool result still on remote")
		}
	}
}

func TestPushSessions_NoChangesAndEmpty(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	writeSessionTree(t, env.claudeDir)

	result, err := env.syncer.PushSessions(ctx, nil)
	if err != nil || len(result.Uploaded)+len(result.Deleted) != 0 {
		t.Fatalf("empty refs: %+v, %v", result, err)
	}

	refs, _ := env.syncer.ResolveSessions([]string{sessC})
	if _, err := env.syncer.PushSessions(ctx, refs); err != nil {
		t.Fatal(err)
	}
	result, err = env.syncer.PushSessions(ctx, refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Uploaded)+len(result.Deleted) != 0 {
		t.Errorf("second push should be a no-op: %+v", result)
	}
}

func TestDetectChangesWithin_ScopesDeletes(t *testing.T) {
	dir := t.TempDir()
	state := NewState()
	for _, p := range []string{"projects/p/a.jsonl", "projects/p/a/x", "projects/p/ab.jsonl", "CLAUDE.md"} {
		state.Files[p] = &FileState{Path: p, Hash: "h"}
	}

	changes, err := state.DetectChangesWithin(dir, []string{"projects/p/a.jsonl", "projects/p/a"})
	if err != nil {
		t.Fatal(err)
	}
	var deleted []string
	for _, c := range changes {
		if c.Action == "delete" {
			deleted = append(deleted, c.Path)
		}
	}
	sort.Strings(deleted)
	if strings.Join(deleted, ",") != "projects/p/a.jsonl,projects/p/a/x" {
		t.Errorf("deleted = %v (must not include ab.jsonl or CLAUDE.md)", deleted)
	}

	// The unscoped variant keeps its historical behavior.
	all, err := state.DetectChanges(dir, []string{"projects/p/a.jsonl"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Errorf("DetectChanges should report all 4 missing files, got %d", len(all))
	}
}

func TestCoveredBy(t *testing.T) {
	roots := []string{"projects", "tasks/", "CLAUDE.md"}
	cases := map[string]bool{
		"projects":            true,
		"projects/p/a.jsonl":  true,
		"tasks/x/1.json":      true,
		"CLAUDE.md":           true,
		"projectsX/a":         false,
		"CLAUDE.md.bak":       false,
		"plugins/x":           false,
		"history.jsonl":       false,
		"projects-other/file": false,
	}
	for p, want := range cases {
		if got := coveredBy(p, roots); got != want {
			t.Errorf("coveredBy(%q) = %v, want %v", p, got, want)
		}
	}
}
