package shared

import (
	"strings"
	"testing"
)

func TestFRPBindingValidation(t *testing.T) {
	for _, b := range []FRPBinding{{ServerID: "server", RawClientID: "id"}, {ServerID: "server", User: "a.b", RawClientID: "c.d"}} {
		if b.Validate() != nil {
			t.Fatalf("rejected %+v", b)
		}
	}
	for _, b := range []FRPBinding{{}, {ServerID: "server"}, {ServerID: "server", RawClientID: " "}, {ServerID: "server", User: "\x00", RawClientID: "id"}, {ServerID: "server", RawClientID: string([]byte{0xff})}, {ServerID: "server", RawClientID: strings.Repeat("x", 129)}} {
		if b.Validate() == nil {
			t.Fatal("invalid binding accepted")
		}
	}
}
