package cliutil

import "testing"

// TestEnvBool pins how COB_* boolean env vars parse. The footgun this guards
// against: an unrecognized value silently flipping behavior. COB_QUIET=no
// must mean "not quiet", not "quiet on". Recognized truthy/falsey spellings
// (incl. yes/no/on/off) map as expected; anything else is treated as unset
// so a typo falls through to the flag/default instead of forcing true.
func TestEnvBool(t *testing.T) {
	truthy := []string{"1", "t", "T", "true", "TRUE", "True", "y", "yes", "YES", "on", "On"}
	falsey := []string{"0", "f", "false", "FALSE", "n", "no", "NO", "off", "Off"}
	unset := []string{"nope", "yep", "2", "enable", "  ", "tru"}

	const name = "COB_TEST_BOOL"
	for _, v := range truthy {
		t.Run("truthy/"+v, func(t *testing.T) {
			t.Setenv(name, v)
			if set, val := envBool(name); !set || !val {
				t.Errorf("envBool(%q) = (set=%v, val=%v), want (true, true)", v, set, val)
			}
		})
	}
	for _, v := range falsey {
		t.Run("falsey/"+v, func(t *testing.T) {
			t.Setenv(name, v)
			if set, val := envBool(name); !set || val {
				t.Errorf("envBool(%q) = (set=%v, val=%v), want (true, false)", v, set, val)
			}
		})
	}
	for _, v := range unset {
		t.Run("unset/"+v, func(t *testing.T) {
			t.Setenv(name, v)
			if set, _ := envBool(name); set {
				t.Errorf("envBool(%q) reported set=true; an unrecognized value must be treated as unset", v)
			}
		})
	}
	t.Run("absent", func(t *testing.T) {
		if set, _ := envBool("COB_DEFINITELY_UNSET_XYZ"); set {
			t.Error("an absent var must report set=false")
		}
	})
}
