package dto

import (
	"reflect"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestPublicSettingsInjectionPayload_IsSparseMap(t *testing.T) {
	typ := reflect.TypeOf(service.PublicSettingsInjectionPayload{})
	if typ.Kind() != reflect.Map {
		t.Fatalf("PublicSettingsInjectionPayload kind = %s, want map", typ.Kind())
	}
	if typ.Key().Kind() != reflect.String {
		t.Fatalf("PublicSettingsInjectionPayload key kind = %s, want string", typ.Key().Kind())
	}
}
