package admission

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

func validRequest(now time.Time) Request {
	return Request{ActivityID: 11, Generation: 2, UserID: 7, RequestID: "request-a", StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Minute), AcceptedAt: now.Add(-time.Second), ProcessingDeadline: now.Add(time.Minute)}
}

func TestValidateRejectsUntrustedOrExpiredTimesBeforeRedis(t *testing.T) {
	s, err := New(noopScripter{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	cases := []struct {
		name   string
		change func(*Request)
		want   error
	}{
		{"accepted before start", func(r *Request) { r.AcceptedAt = r.StartsAt.Add(-time.Nanosecond) }, ErrAcceptedOutsideWindow},
		{"accepted at end", func(r *Request) { r.ProcessingDeadline = r.EndsAt.Add(time.Second); r.AcceptedAt = r.EndsAt }, ErrAcceptedOutsideWindow},
		{"deadline expired", func(r *Request) { r.ProcessingDeadline = now }, ErrProcessingDeadline},
		{"missing request", func(r *Request) { r.RequestID = "" }, ErrInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRequest(now)
			tc.change(&r)
			_, err := s.Admit(context.Background(), r)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestActivityKeysIncludeActivityAndGeneration(t *testing.T) {
	s, err := New(noopScripter{}, "local-life:admission")
	if err != nil {
		t.Fatal(err)
	}
	keys := s.activityKeys(12, 34)
	if len(keys) != 6 || keys[0] != "local-life:admission:{12:34}:stock" || keys[2] != "local-life:admission:{12:34}:users" || keys[3] != "local-life:admission:{12:34}:requests" || keys[4] != "local-life:admission:{12:34}:stock:releases" || keys[5] != "local-life:admission:{12:34}:meta" {
		t.Fatalf("keys = %#v", keys)
	}
}

func TestLuaResultMappingAndOwnershipArguments(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	client := &scriptedScripter{result: string(Admitted)}
	s, err := New(client, "test")
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	r := validRequest(now)
	if got, err := s.Admit(context.Background(), r); err != nil || got != Admitted {
		t.Fatalf("admit = %q, %v", got, err)
	}
	if client.keys[0] != "test:{11:2}:stock" || client.args[0] != "7" || client.args[1] != "request-a" || len(client.args) != 7 || client.args[2] != "2" || client.args[3] != strconv.FormatInt(r.AcceptedAt.Unix(), 10) {
		t.Fatalf("Lua input keys=%v args=%v", client.keys, client.args)
	}
	client.result = "SOLD_OUT"
	if _, err := s.Admit(context.Background(), r); !errors.Is(err, ErrSoldOut) {
		t.Fatalf("sold out = %v", err)
	}
	client.result = string(Released)
	if got, err := s.Release(context.Background(), 11, 2, 7, "request-a"); err != nil || got != Released {
		t.Fatalf("release = %q, %v", got, err)
	}
	client.result = "ORDER_COMMITTED"
	if _, err := s.Release(context.Background(), 11, 2, 7, "request-a"); !errors.Is(err, ErrOrderAlreadyCommitted) {
		t.Fatalf("committed release = %v", err)
	}
	for result, want := range map[string]error{"MISSING": ErrStateMissing, "PARTIAL": ErrStatePartial, "INCONSISTENT": ErrStatePartial} {
		client.result = result
		if got, err := s.Release(context.Background(), 11, 2, 7, "request-a"); got != Noop || !errors.Is(err, want) {
			t.Fatalf("release script result=%s got=%s err=%v", result, got, err)
		}
	}
	client.result = "UPDATED"
	if err := s.SetOpen(context.Background(), 11, 2, false); err != nil {
		t.Fatal(err)
	}
	if client.keys[1] != "test:{11:2}:open" || client.args[0] != "0" {
		t.Fatalf("switch Lua input keys=%v args=%v", client.keys, client.args)
	}
}
