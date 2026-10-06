package dbtunnel_test

import (
	"testing"

	"github.com/Itema-as/iidp/internal/dbtunnel"
	"github.com/Itema-as/iidp/internal/render"
)

func TestTheHighestQualifyingLevelPicksTheRole(t *testing.T) {
	for _, tc := range []struct {
		name       string
		permission string
		access     render.DatabaseAccess
		readOnly   bool
		want       render.AccessRole
	}{
		{"admin gets read-write where readWrite is admin", "admin", render.DatabaseAccess{ReadWrite: "admin", ReadOnly: "pull"}, false, render.ReadWriteRole},
		{"maintain falls back to read-only below readWrite admin", "maintain", render.DatabaseAccess{ReadWrite: "admin", ReadOnly: "pull"}, false, render.ReadOnlyRole},
		{"push gets read-only where only readOnly is push", "push", render.DatabaseAccess{ReadWrite: "none", ReadOnly: "push"}, false, render.ReadOnlyRole},
		{"admin gets read-only where only readOnly is push", "admin", render.DatabaseAccess{ReadWrite: "none", ReadOnly: "push"}, false, render.ReadOnlyRole},
		{"push gets read-write on staging's defaults", "push", render.DatabaseAccess{ReadWrite: "push", ReadOnly: "none"}, false, render.ReadWriteRole},
		{"--read-only steps read-write down", "push", render.DatabaseAccess{ReadWrite: "push", ReadOnly: "pull"}, true, render.ReadOnlyRole},
		{"--read-only for a developer who only qualifies read-only", "pull", render.DatabaseAccess{ReadWrite: "push", ReadOnly: "pull"}, true, render.ReadOnlyRole},
	} {
		got, ok := dbtunnel.ChooseRole(tc.permission, tc.access, tc.readOnly)
		if !ok || got != tc.want {
			t.Errorf("%s: ChooseRole(%s, %+v, readOnly %v) = %v, %v; want %v", tc.name, tc.permission, tc.access, tc.readOnly, got, ok, tc.want)
		}
	}
}

func TestNobodyBelowTheLevelsGetsARole(t *testing.T) {
	for _, tc := range []struct {
		name       string
		permission string
		access     render.DatabaseAccess
		readOnly   bool
	}{
		{"none lets nobody in, admin included", "admin", render.DatabaseAccess{ReadWrite: "none", ReadOnly: "none"}, false},
		{"pull is below push", "pull", render.DatabaseAccess{ReadWrite: "push", ReadOnly: "none"}, false},
		{"maintain is below admin", "maintain", render.DatabaseAccess{ReadWrite: "admin", ReadOnly: "admin"}, false},
		{"--read-only where read-only is none", "admin", render.DatabaseAccess{ReadWrite: "push", ReadOnly: "none"}, true},
		{"no permission at all", "", render.DatabaseAccess{ReadWrite: "pull", ReadOnly: "pull"}, false},
	} {
		if got, ok := dbtunnel.ChooseRole(tc.permission, tc.access, tc.readOnly); ok {
			t.Errorf("%s: ChooseRole(%s, %+v, readOnly %v) = %v, want no role", tc.name, tc.permission, tc.access, tc.readOnly, got)
		}
	}
}
