package shell

import (
	"context"
	"testing"
)

func TestRunEmptyEnvironmentDoesNotInherit(t *testing.T) {
	t.Setenv("LOOPER_TEST_INHERITED_SECRET", "inherited")
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "nil preserves legacy inheritance", want: "inherited"},
		{name: "empty explicitly isolates", env: map[string]string{}, want: "absent"},
		{name: "nonempty replaces", env: map[string]string{"LOOPER_TEST_EXPLICIT": "present"}, want: "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Run(context.Background(), Options{Command: "/bin/sh", Args: []string{"-c", `printf %s "${LOOPER_TEST_INHERITED_SECRET-absent}"`}, Env: tc.env})
			if err != nil || result.Stdout != tc.want {
				t.Fatalf("child output = %q, error = %v; want %q", result.Stdout, err, tc.want)
			}
		})
	}
}
