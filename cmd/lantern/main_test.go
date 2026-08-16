package main

import (
	"reflect"
	"testing"
)

// TestReorderArgsHandlesFlagsAfterPositionals is the regression test for a
// bug found running `lantern discover` against a real cluster: Go's
// flag.FlagSet.Parse stops at the first non-flag argument, so
// `lantern discover - -team unassigned` silently treated "-team" and
// "unassigned" as file paths instead of a flag, producing a misleading
// "stat -team: no such file or directory" error.
func TestReorderArgsHandlesFlagsAfterPositionals(t *testing.T) {
	cases := []struct {
		name           string
		args           []string
		wantFlags      []string
		wantPositional []string
	}{
		{
			name:           "the exact repro: stdin marker then a value flag",
			args:           []string{"-", "-team", "unassigned"},
			wantFlags:      []string{"-team", "unassigned"},
			wantPositional: []string{"-"},
		},
		{
			name:           "flags first (already worked)",
			args:           []string{"-team", "platform", "-"},
			wantFlags:      []string{"-team", "platform"},
			wantPositional: []string{"-"},
		},
		{
			name:           "positional, then flag, then another positional",
			args:           []string{"a.yaml", "-ns", "dev,prod", "b.yaml"},
			wantFlags:      []string{"-ns", "dev,prod"},
			wantPositional: []string{"a.yaml", "b.yaml"},
		},
		{
			name:           "bool flag needs no value and does not swallow the next token",
			args:           []string{"-all", "./manifests"},
			wantFlags:      []string{"-all"},
			wantPositional: []string{"./manifests"},
		},
		{
			name:           "equals form is left untouched, not merged with the next token",
			args:           []string{"-stack=stack.yaml", "services.yaml"},
			wantFlags:      []string{"-stack=stack.yaml"},
			wantPositional: []string{"services.yaml"},
		},
		{
			name:           "-- stops flag recognition for everything after it",
			args:           []string{"--", "-team", "unassigned"},
			wantFlags:      nil,
			wantPositional: []string{"-team", "unassigned"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			flags, positional := reorderArgs(c.args)
			if !reflect.DeepEqual(flags, c.wantFlags) {
				t.Errorf("flags = %#v, want %#v", flags, c.wantFlags)
			}
			if !reflect.DeepEqual(positional, c.wantPositional) {
				t.Errorf("positional = %#v, want %#v", positional, c.wantPositional)
			}
		})
	}
}
