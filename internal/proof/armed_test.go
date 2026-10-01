package proof

import (
	"fmt"
	"testing"
)

// Each constructor names its source: when an armed run is
// questioned, the answer is which boundary granted it. If this
// fails, intent arrives without provenance.
func TestArmedRunNamesSource(t *testing.T) {
	if got := armedFromFlag().Source(); got != "flag" {
		t.Errorf("flag arming source = %q, want flag", got)
	}
	if got := armedFromEnv().Source(); got != "env" {
		t.Errorf("env arming source = %q, want env", got)
	}
}

// The zero value is nothing: without a constructor there is no
// arming. If this fails, intent can be minted from thin air.
func TestArmedRunZeroIsNothing(t *testing.T) {
	var arm ArmedRun
	if arm != nil {
		t.Errorf("zero ArmedRun = %v, want nil", arm)
	}
}

// The boundary names its source: flag beats env, either beats
// dry-run, nothing mints nothing. If this fails, intent arrives
// without provenance — or dry-run stopped being the default.
func TestArmNamesSource(t *testing.T) {
	cases := []struct {
		name string
		flag bool
		env  bool
		want string // "" means no arming
	}{
		{"neither arms nothing", false, false, ""},
		{"flag arms from flag", true, false, "flag"},
		{"env arms from env", false, true, "env"},
		{"flag beats env", true, true, "flag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := Arm(tc.flag, tc.env)
			if tc.want == "" {
				if a != nil {
					t.Errorf("Arm(%v, %v) = %v, want nil", tc.flag, tc.env, a)
				}
				return
			}
			if a == nil {
				t.Fatalf("Arm(%v, %v) = nil, want %q", tc.flag, tc.env, tc.want)
			}
			if got := a.Source(); got != tc.want {
				t.Errorf("Arm(%v, %v) source = %q, want %q", tc.flag, tc.env, got, tc.want)
			}
		})
	}
}

// A stage takes its evidence as an argument: without ArmedRun
// there is no call. If this fails, arming stopped meaning it.
func ExampleArm() {
	arm := armedFromFlag()
	collectIfArmedRun(arm)
	// Output: collecting (armed via flag)
}

func collectIfArmedRun(a ArmedRun) {
	fmt.Println("collecting (armed via " + a.Source() + ")")
}
