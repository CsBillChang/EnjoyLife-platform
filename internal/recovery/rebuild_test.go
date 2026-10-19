package recovery

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/CsBillChang/local-life-platform/internal/admission"
)

type fakeRebuildStore struct {
	calls []string
	fail  string
}

func (f *fakeRebuildStore) SetOpen(context.Context, uint64, uint64, bool) error { return nil }
func (f *fakeRebuildStore) record(name string) error {
	f.calls = append(f.calls, name)
	if f.fail == name {
		return errors.New("injected Redis failure")
	}
	return nil
}
func (f *fakeRebuildStore) InstallRebuild(context.Context, admission.RebuildSnapshot, string) error {
	return f.record("install")
}
func (f *fakeRebuildStore) VerifyRebuild(context.Context, admission.RebuildSnapshot, string) error {
	return f.record("verify")
}
func (f *fakeRebuildStore) PublishRebuild(context.Context, admission.RebuildSnapshot, string) error {
	return f.record("publish")
}

func TestPublicationOrderingAndFailpoints(t *testing.T) {
	for _, phase := range []Phase{PhaseInstalled, PhaseVerified, PhaseRedisOpen, PhaseDone} {
		t.Run(string(phase), func(t *testing.T) {
			store := &fakeRebuildStore{}
			svc := NewService(nil, store, nil)
			injected := errors.New("crash at checkpoint")
			var checkpoints []Phase
			job := Task{ID: 5, ActivityID: 9, Generation: 2, Phase: PhaseNormalized, Status: "RUNNING"}
			got, err := svc.publish(context.Background(), job, admission.RebuildSnapshot{}, func(current Task, next Phase) (Task, error) {
				checkpoints = append(checkpoints, next)
				current.Phase = next
				if next == phase {
					return current, injected
				}
				return current, nil
			})
			if phase != PhaseDone && !errors.Is(err, injected) {
				t.Fatalf("checkpoint error=%v", err)
			}
			want := []string{"install"}
			if phaseRank(phase) >= phaseRank(PhaseVerified) {
				want = append(want, "verify")
			}
			if phaseRank(phase) >= phaseRank(PhaseRedisOpen) {
				want = append(want, "publish")
			}
			if !reflect.DeepEqual(store.calls, want) {
				t.Fatalf("operations=%v want=%v", store.calls, want)
			}
			if got.Status != "RUNNING" {
				t.Fatal("Redis publication incorrectly completed MySQL task")
			}
		})
	}
}

func TestFailedRedisStageCannotPublishOrCheckpoint(t *testing.T) {
	for _, fail := range []string{"install", "verify", "publish"} {
		store := &fakeRebuildStore{fail: fail}
		svc := NewService(nil, store, nil)
		var checkpoints []Phase
		_, err := svc.publish(context.Background(), Task{ID: 1, Generation: 2}, admission.RebuildSnapshot{}, func(task Task, phase Phase) (Task, error) { checkpoints = append(checkpoints, phase); return task, nil })
		if err == nil {
			t.Fatal("Redis failure ignored")
		}
		if len(checkpoints) != len(store.calls)-1 {
			t.Fatal("failed operation checkpointed as complete")
		}
	}
}

func TestRebuildInputAndStableToken(t *testing.T) {
	service := NewService(nil, &fakeRebuildStore{}, nil)
	if _, err := service.Start(context.Background(), 0); !errors.Is(err, ErrInvalidRebuild) {
		t.Fatal(err)
	}
	if _, err := service.Resume(context.Background(), 0); !errors.Is(err, ErrInvalidRebuild) {
		t.Fatal(err)
	}
	if rebuildToken(Task{ID: 12, Generation: 3}) != "rebuild:12:3" {
		t.Fatal("unstable token")
	}
	if taskSource(12, 3) == taskSource(12, 4) {
		t.Fatal("generation task identity collision")
	}
	if phaseRank("unknown") != 0 || phaseRank(PhaseDone) <= phaseRank(PhaseRedisOpen) {
		t.Fatal("bad phase ordering")
	}
}
