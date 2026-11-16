package config

import (
	"strings"
	"testing"
)

// M1-G7: TM_QUEUE_ERROR_MAX_ATTEMPTS, the attempt cap for items that cannot be computed (policy / decrypt / compute error). Default 5,
// validated 1..50. The property: 0 would let an item that can never be computed requeue forever — the defect the cap exists to remove —
// so the loader must refuse it (and the unbounded and garbage cases) rather than start a control-plane that never settles such items.

func TestQueueErrorMaxAttempts_Default(t *testing.T) {
	c := Defaults()
	if c.Dials.QueueErrorMaxAttempts != 5 {
		t.Errorf("default = %d, want 5", c.Dials.QueueErrorMaxAttempts)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
	loaded, err := Load(func(string) string { return "" })
	if err != nil || loaded.Dials.QueueErrorMaxAttempts != 5 {
		t.Errorf("an unset TM_QUEUE_ERROR_MAX_ATTEMPTS must load as 5, got %d (err %v)", loaded.Dials.QueueErrorMaxAttempts, err)
	}
}

func TestQueueErrorMaxAttempts_LoadsFromEnv(t *testing.T) {
	c, err := Load(func(k string) string {
		if k == "TM_QUEUE_ERROR_MAX_ATTEMPTS" {
			return "9"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Dials.QueueErrorMaxAttempts != 9 {
		t.Errorf("env not applied: %d", c.Dials.QueueErrorMaxAttempts)
	}
}

func TestQueueErrorMaxAttempts_RejectsTheUnboundedAndGarbage(t *testing.T) {
	for _, v := range []string{"0", "-1", "abc", "51", "1000", "5.5", " "} {
		_, err := Load(func(k string) string {
			if k == "TM_QUEUE_ERROR_MAX_ATTEMPTS" {
				return v
			}
			return ""
		})
		if err == nil {
			t.Errorf("TM_QUEUE_ERROR_MAX_ATTEMPTS=%q must be rejected at startup", v)
		} else if !strings.Contains(err.Error(), "config:") {
			t.Errorf("TM_QUEUE_ERROR_MAX_ATTEMPTS=%q: unexpected error shape: %v", v, err)
		}
	}
	// A Config built any other way (a test, a future file source) cannot carry 0 or >50 past Validate either.
	for _, n := range []int{0, -3, 51} {
		c := Defaults()
		c.Dials.QueueErrorMaxAttempts = n
		if err := c.Validate(); err == nil {
			t.Errorf("Validate must reject queue_error_max_attempts=%d", n)
		}
	}
	// CONTROL: the boundary values ARE accepted, so the rejections above are about the value, not the key.
	for _, ok := range []string{"1", "5", "50"} {
		if _, err := Load(func(k string) string {
			if k == "TM_QUEUE_ERROR_MAX_ATTEMPTS" {
				return ok
			}
			return ""
		}); err != nil {
			t.Errorf("TM_QUEUE_ERROR_MAX_ATTEMPTS=%s must be accepted: %v", ok, err)
		}
	}
}
