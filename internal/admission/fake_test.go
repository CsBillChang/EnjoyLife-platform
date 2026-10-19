package admission

import (
	"context"
	"github.com/redis/go-redis/v9"
)

// noopScripter is enough for validation tests: calling it means validation did
// not reject a request as intended.
type noopScripter struct{}

func (noopScripter) Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
	return redis.NewCmd(ctx, "EVAL")
}
func (noopScripter) EvalSha(ctx context.Context, sha1 string, keys []string, args ...interface{}) *redis.Cmd {
	return redis.NewCmd(ctx, "EVALSHA")
}
func (noopScripter) EvalRO(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
	return redis.NewCmd(ctx, "EVAL_RO")
}
func (noopScripter) EvalShaRO(ctx context.Context, sha1 string, keys []string, args ...interface{}) *redis.Cmd {
	return redis.NewCmd(ctx, "EVALSHA_RO")
}
func (noopScripter) ScriptExists(ctx context.Context, hashes ...string) *redis.BoolSliceCmd {
	return redis.NewBoolSliceCmd(ctx, "SCRIPT", "EXISTS")
}
func (noopScripter) ScriptLoad(ctx context.Context, script string) *redis.StringCmd {
	return redis.NewStringCmd(ctx, "SCRIPT", "LOAD")
}

type scriptedScripter struct {
	result string
	keys   []string
	args   []interface{}
}

func (s *scriptedScripter) command(ctx context.Context, keys []string, args []interface{}) *redis.Cmd {
	s.keys = append([]string(nil), keys...)
	s.args = append([]interface{}(nil), args...)
	cmd := redis.NewCmd(ctx, "EVALSHA")
	cmd.SetVal(s.result)
	return cmd
}
func (s *scriptedScripter) Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
	return s.command(ctx, keys, args)
}
func (s *scriptedScripter) EvalSha(ctx context.Context, sha1 string, keys []string, args ...interface{}) *redis.Cmd {
	return s.command(ctx, keys, args)
}
func (s *scriptedScripter) EvalRO(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
	return s.command(ctx, keys, args)
}
func (s *scriptedScripter) EvalShaRO(ctx context.Context, sha1 string, keys []string, args ...interface{}) *redis.Cmd {
	return s.command(ctx, keys, args)
}
func (s *scriptedScripter) ScriptExists(ctx context.Context, hashes ...string) *redis.BoolSliceCmd {
	return redis.NewBoolSliceCmd(ctx, "SCRIPT", "EXISTS")
}
func (s *scriptedScripter) ScriptLoad(ctx context.Context, script string) *redis.StringCmd {
	return redis.NewStringCmd(ctx, "SCRIPT", "LOAD")
}
