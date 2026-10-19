package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func validRequest() Request {
	return Request{ActivityID: 17, UserID: 23, Total: Bucket{Capacity: 10, RefillPerSecond: 2}, User: Bucket{Capacity: 3, RefillPerSecond: 1}, Cost: 1}
}

func TestNewAndKeysUseOneActivityHashTag(t *testing.T) {
	if _, err := New(&scriptedScripter{}, "bad:{prefix}"); !errors.Is(err, ErrInvalidPrefix) {
		t.Fatalf("prefix error = %v", err)
	}
	s, err := New(&scriptedScripter{}, "local-life:limit")
	if err != nil {
		t.Fatal(err)
	}
	keys := s.keys(17, 23)
	if len(keys) != 2 || keys[0] != "local-life:limit:{17}:total" || keys[1] != "local-life:limit:{17}:user:23" {
		t.Fatalf("keys = %#v", keys)
	}
}

func TestAllowValidatesBoundsBeforeRedis(t *testing.T) {
	s, err := New(&scriptedScripter{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	cases := []Request{
		{ActivityID: 1, UserID: 1, Total: Bucket{Capacity: 1, RefillPerSecond: 1}, User: Bucket{Capacity: 1, RefillPerSecond: 1}},
		{ActivityID: 1, UserID: 1, Total: Bucket{Capacity: maxCapacity + 1, RefillPerSecond: 1}, User: Bucket{Capacity: 1, RefillPerSecond: 1}, Cost: 1},
		{ActivityID: 1, UserID: 1, Total: Bucket{Capacity: 1, RefillPerSecond: 0}, User: Bucket{Capacity: 1, RefillPerSecond: 1}, Cost: 1},
		{ActivityID: 1, UserID: 1, Total: Bucket{Capacity: 1, RefillPerSecond: 1}, User: Bucket{Capacity: 1, RefillPerSecond: 1}, Cost: 2},
	}
	for _, request := range cases {
		if _, err := s.Allow(context.Background(), request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Allow(%+v) = %v", request, err)
		}
	}
}

func TestAllowMapsDecisionAndUsesBothBuckets(t *testing.T) {
	client := &scriptedScripter{result: []interface{}{int64(0), int64(1500)}}
	s, err := New(client, "test")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Allow(context.Background(), validRequest())
	if err != nil || got.Allowed || got.RetryAfter != 1500*time.Millisecond {
		t.Fatalf("decision = %+v, %v", got, err)
	}
	if client.keys[0] != "test:{17}:total" || client.keys[1] != "test:{17}:user:23" {
		t.Fatalf("keys = %v", client.keys)
	}
	if len(client.args) != 6 || client.args[0] != uint64(10) || client.args[2] != uint64(3) || client.args[4] != uint64(1) {
		t.Fatalf("args = %#v", client.args)
	}
	client.result = []interface{}{"1", "0"}
	got, err = s.Allow(context.Background(), validRequest())
	if err != nil || !got.Allowed || got.RetryAfter != 0 {
		t.Fatalf("allowed = %+v, %v", got, err)
	}
}

func TestAllowReturnsRedisErrorForFailClosedCaller(t *testing.T) {
	lost := errors.New("redis unavailable")
	s, err := New(&scriptedScripter{err: lost}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Allow(context.Background(), validRequest()); !errors.Is(err, lost) {
		t.Fatalf("error = %v", err)
	}
}
