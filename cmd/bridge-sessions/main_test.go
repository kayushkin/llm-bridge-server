package main

import (
	"flag"
	"reflect"
	"testing"
)

func TestFlagsMayFollowThePositionalArguments(t *testing.T) {
	flags := flag.NewFlagSet("t", flag.ContinueOnError)
	limit := flags.Int("limit", 20, "")
	positional, err := parseInterspersed(flags, []string{"acme widgets", "--limit", "5"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(positional, []string{"acme widgets"}) || *limit != 5 {
		t.Errorf("positional %q, limit %d; want [acme widgets], 5", positional, *limit)
	}
}

func TestTheServerIsTheOneThatSetTheSessionsActionsURL(t *testing.T) {
	t.Setenv(actionsURLEnvironmentVariable, "http://localhost:8160/sessions/br_1/actions")
	t.Setenv(postingTokenEnvironmentVariable, "token")
	t.Setenv(serviceTokenEnvironmentVariable, "")
	c, err := newClient("")
	if err != nil {
		t.Fatal(err)
	}
	if c.base != "http://localhost:8160" {
		t.Errorf("base %q, want http://localhost:8160", c.base)
	}
	t.Setenv(postingTokenEnvironmentVariable, "")
	if _, err := newClient(""); err == nil {
		t.Error("no credential: want an error")
	}
}
