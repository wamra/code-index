package main

import (
	"io"
	"reflect"
	"testing"
)

func TestParseInterspersedDoubleDash(t *testing.T) {
	for _, c := range []struct {
		args []string
		want []string
		json bool
	}{
		{[]string{"foo"}, []string{"foo"}, false},
		{[]string{"--json", "foo"}, []string{"foo"}, true},
		{[]string{"foo", "--json"}, []string{"foo"}, true},
		{[]string{"--json", "--", "-x"}, []string{"-x"}, true},
		{[]string{"--", "--json"}, []string{"--json"}, false},
		{[]string{"--json", "--", "a", "-b", "--json"}, []string{"a", "-b", "--json"}, true},
		{[]string{"--"}, nil, false},
	} {
		fs := newFlags("t", io.Discard)
		j := fs.Bool("json", false, "")
		got, err := parseInterspersed(fs, c.args)
		if err != nil || !reflect.DeepEqual(got, c.want) || *j != c.json {
			t.Errorf("%v: got %v json=%v err=%v, want %v json=%v", c.args, got, *j, err, c.want, c.json)
		}
	}
}
