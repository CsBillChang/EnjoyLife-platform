package config

import "testing"

func TestLoadProfilingDefaultsDisabled(t *testing.T) {
	t.Setenv("PPROF_MUTEX_FRACTION", "")
	t.Setenv("PPROF_BLOCK_RATE", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PprofMutexRate != 0 || cfg.PprofBlockRate != 0 {
		t.Fatalf("profiling defaults = mutex %d block %d, want both disabled", cfg.PprofMutexRate, cfg.PprofBlockRate)
	}
}

func TestLoadProfilingDiagnosticRates(t *testing.T) {
	t.Setenv("PPROF_MUTEX_FRACTION", "1")
	t.Setenv("PPROF_BLOCK_RATE", "1000000")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PprofMutexRate != 1 || cfg.PprofBlockRate != 1_000_000 {
		t.Fatalf("profiling rates = mutex %d block %d", cfg.PprofMutexRate, cfg.PprofBlockRate)
	}
}

func TestLoadRejectsInvalidProfilingRates(t *testing.T) {
	for _, item := range []struct {
		name  string
		value string
	}{
		{name: "PPROF_MUTEX_FRACTION", value: "-1"},
		{name: "PPROF_BLOCK_RATE", value: "not-a-number"},
		{name: "PPROF_BLOCK_RATE", value: "1000000001"},
	} {
		t.Run(item.name+"="+item.value, func(t *testing.T) {
			t.Setenv("PPROF_MUTEX_FRACTION", "0")
			t.Setenv("PPROF_BLOCK_RATE", "0")
			t.Setenv(item.name, item.value)
			if _, err := Load(); err == nil {
				t.Fatal("Load() succeeded, want invalid profiling rate error")
			}
		})
	}
}
