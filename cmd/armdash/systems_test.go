package main

import (
	"strings"
	"testing"

	"armdash/internal/system"
)

// The navbar, and with it the default page, follows registration order.
func TestSystemsRegisterInNavbarOrder(t *testing.T) {
	var ids []string
	for _, s := range system.New() {
		ids = append(ids, s.ID())
	}
	if got := strings.Join(ids, ", "); got != "host, fritzhome" {
		t.Errorf("registered %s, want host, fritzhome", got)
	}
}
