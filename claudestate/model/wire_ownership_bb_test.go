package model_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/MadAppGang/magmux/claudestate/model"
)

func TestBBModelLeavesJSONPolicyToItsHost(t *testing.T) {
	seen := make(map[reflect.Type]bool)
	var visit func(reflect.Type)
	visit = func(typ reflect.Type) {
		if seen[typ] {
			return
		}
		seen[typ] = true
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			visit(typ.Elem())
		case reflect.Struct:
			if typ.PkgPath() != "github.com/MadAppGang/magmux/claudestate/model" {
				return
			}
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				if field.PkgPath != "" {
					continue
				}
				if tag, ok := field.Tag.Lookup("json"); ok {
					t.Errorf("library owns host JSON policy: %s.%s json:%q", typ.Name(), field.Name, tag)
				}
				visit(field.Type)
			}
		}
	}
	for _, v := range []any{model.Fleet{}, model.Problem{}, model.Turn{}, model.ShellState{}, model.WatchTarget{}} {
		visit(reflect.TypeOf(v))
	}
	marshaler := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	unmarshaler := reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	for _, v := range []any{
		model.KindUnknown, model.StatusUnknown, model.WaitingUnknown, model.LivenessUnknown,
		model.ConfidenceUnknown, model.EvidenceUnknown, model.ShellMarkUnknown, model.SourceUnknown,
		model.SourceStateUnknown, model.TargetUnknown, model.ScopeUnknown, model.ProblemUnknown,
	} {
		typ := reflect.TypeOf(v)
		for _, candidate := range []reflect.Type{typ, reflect.PointerTo(typ)} {
			if candidate.Implements(marshaler) || candidate.Implements(unmarshaler) {
				t.Errorf("enum %s implements host JSON policy", candidate)
			}
		}
	}
}
