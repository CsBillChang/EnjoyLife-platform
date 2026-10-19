package ratelimit

import (
	"context"

	"github.com/redis/go-redis/v9"
)

type scriptedScripter struct {
	result interface{}
	err    error
	keys   []string
	args   []interface{}
}

func (s *scriptedScripter) command(ctx context.Context, keys []string, args []interface{}) *redis.Cmd {
	s.keys = append([]string(nil), keys...)
	s.args = append([]interface{}(nil), args...)
	cmd := redis.NewCmd(ctx, "EVALSHA")
	cmd.SetVal(s.result)
	if s.err != nil {
		cmd.SetErr(s.err)
	}
	return cmd
}

func (s *scriptedScripter) Eval(ctx context.Context, _ string, keys []string, args ...interface{}) *redis.Cmd {
	return s.command(ctx, keys, args)
}
func (s *scriptedScripter) EvalSha(ctx context.Context, _ string, keys []string, args ...interface{}) *redis.Cmd {
	return s.command(ctx, keys, args)
}
func (s *scriptedScripter) EvalRO(ctx context.Context, _ string, keys []string, args ...interface{}) *redis.Cmd {
	return s.command(ctx, keys, args)
}
func (s *scriptedScripter) EvalShaRO(ctx context.Context, _ string, keys []string, args ...interface{}) *redis.Cmd {
	return s.command(ctx, keys, args)
}
func (s *scriptedScripter) ScriptExists(ctx context.Context, _ ...string) *redis.BoolSliceCmd {
	return redis.NewBoolSliceCmd(ctx, "SCRIPT", "EXISTS")
}
func (s *scriptedScripter) ScriptLoad(ctx context.Context, _ string) *redis.StringCmd {
	return redis.NewStringCmd(ctx, "SCRIPT", "LOAD")
}
