package admission

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRebuildSnapshotInputAndArguments(t *testing.T) {
	snapshot := RebuildSnapshot{ActivityID: 1, Generation: 2, StockHint: 3, StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour), Owners: []CommittedOwner{{UserID: 4, RequestID: "request"}}}
	args, err := rebuildArgs(snapshot, "task-1")
	if err != nil || len(args) != 10 || args[1] != "3" || args[8] != "4" {
		t.Fatalf("args=%v err=%v", args, err)
	}
	for _, mutate := range []func(*RebuildSnapshot){func(s *RebuildSnapshot) { s.ActivityID = 0 }, func(s *RebuildSnapshot) { s.Generation = 0 }, func(s *RebuildSnapshot) { s.Owners = append(s.Owners, s.Owners[0]) }} {
		changed := snapshot
		mutate(&changed)
		if _, err := rebuildArgs(changed, "task-1"); !errors.Is(err, ErrInvalidRequest) {
			t.Fatal(err)
		}
	}
	snapshot.Owners = make([]CommittedOwner, AtomicRebuildOwnerLimit+1)
	if _, err := rebuildArgs(snapshot, "task-1"); !errors.Is(err, ErrSnapshotTooLarge) {
		t.Fatal(err)
	}
}

func TestRebuildScriptResultsAndGenerationKeys(t *testing.T) {
	client := &scriptedScripter{result: "INSTALLED"}
	store, err := New(client, "test")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := RebuildSnapshot{ActivityID: 1, Generation: 2, StockHint: 3, StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour)}
	if err := store.InstallRebuild(context.Background(), snapshot, "task-1"); err != nil {
		t.Fatal(err)
	}
	if len(client.keys) != 7 || client.keys[6] != "test:{1:2}:stock:rebuild" {
		t.Fatal(client.keys)
	}
	for _, tc := range []struct {
		value string
		want  error
	}{{"CONFLICT", ErrRebuildConflict}, {"MISMATCH", ErrSnapshotMismatch}, {"unknown", ErrUnexpectedScriptResult}} {
		client.result = tc.value
		if err := store.VerifyRebuild(context.Background(), snapshot, "task-1"); !errors.Is(err, tc.want) {
			t.Fatal(err)
		}
	}
}
