package version

import "testing"

func TestStringRendersInjectedVersion(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })

	Version = "v1.2.3"
	if got := String(); got != "v1.2.3" {
		t.Fatalf("String() = %q, want v1.2.3", got)
	}
}

func TestStringFallsBackForDevAndEmptyBuilds(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"default dev build", "dev", "dev"},
		{"empty -ldflags value", "", "dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			Version = tc.in
			if got := String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}
