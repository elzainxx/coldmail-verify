package config

import (
	"strings"
	"testing"
	"time"
)

// ADR-0017: the verification_raw partition-maintenance dials. The property: zero headroom is the
// cliff, so the loader must refuse it (and the unbounded/garbage cases) rather than start a
// control-plane that will silently run out of partitions.

func TestRawPartitionDialsDefaults(t *testing.T) {
	c := Defaults()
	if c.Dials.RawPartitionMonthsAhead != 3 {
		t.Errorf("default months-ahead = %d, want 3", c.Dials.RawPartitionMonthsAhead)
	}
	if c.RawPartitionMaintainInterval() != 6*time.Hour {
		t.Errorf("default maintain interval = %s, want 6h", c.RawPartitionMaintainInterval())
	}
	if err := c.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

func TestRawPartitionDialsLoadFromEnv(t *testing.T) {
	env := map[string]string{
		"TM_RAW_PARTITION_MONTHS_AHEAD":              "6",
		"TM_RAW_PARTITION_MAINTAIN_INTERVAL_SECONDS": "600",
	}
	c, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Dials.RawPartitionMonthsAhead != 6 || c.RawPartitionMaintainInterval() != 10*time.Minute {
		t.Errorf("env not applied: months=%d interval=%s", c.Dials.RawPartitionMonthsAhead, c.RawPartitionMaintainInterval())
	}
}

func TestRawPartitionDialsRejectTheCliffAndGarbage(t *testing.T) {
	bad := []struct{ k, v string }{
		{"TM_RAW_PARTITION_MONTHS_AHEAD", "0"}, // zero headroom IS the cliff
		{"TM_RAW_PARTITION_MONTHS_AHEAD", "-1"},
		{"TM_RAW_PARTITION_MONTHS_AHEAD", "abc"},
		{"TM_RAW_PARTITION_MONTHS_AHEAD", "121"}, // over the migration's own cap
		{"TM_RAW_PARTITION_MAINTAIN_INTERVAL_SECONDS", "0"},
		{"TM_RAW_PARTITION_MAINTAIN_INTERVAL_SECONDS", "-5"},
	}
	for _, b := range bad {
		_, err := Load(func(k string) string {
			if k == b.k {
				return b.v
			}
			return ""
		})
		if err == nil {
			t.Errorf("%s=%q must be rejected at startup", b.k, b.v)
		} else if !strings.Contains(err.Error(), "config:") {
			t.Errorf("%s=%q: unexpected error shape: %v", b.k, b.v, err)
		}
	}
	// Control: the boundary values ARE accepted, so the rejections above are about the value, not the key.
	for _, ok := range []string{"1", "120"} {
		if _, err := Load(func(k string) string {
			if k == "TM_RAW_PARTITION_MONTHS_AHEAD" {
				return ok
			}
			return ""
		}); err != nil {
			t.Errorf("TM_RAW_PARTITION_MONTHS_AHEAD=%s must be accepted: %v", ok, err)
		}
	}
}
