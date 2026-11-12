package store

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestParseLocation(t *testing.T) {
	for in, want := range map[string]Location{
		"internal/store/items.go":  {Path: "internal/store/items.go"},
		"a.go:12":                  {Path: "a.go", Line: 12},
		"a.go:12-20":               {Path: "a.go", Line: 12, EndLine: 20},
		"a.go:12:5":                {Path: "a.go", Line: 12},
		"src/a.ts#L3-L9":           {Path: "src/a.ts", Line: 3, EndLine: 9},
		"src/a.ts#L3":              {Path: "src/a.ts", Line: 3},
		"dir with:colon/file.go:7": {Path: "dir with:colon/file.go", Line: 7},
	} {
		got, err := ParseLocation(in)
		if err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
}

func TestItemLocations(t *testing.T) {
	s := newStore(t)
	var in BulkInput
	if err := json.Unmarshal([]byte(`{"items":[{"title":"SQL built by hand","type":"security",
		"locations":["internal/store/items.go:120-134", {"path":"./internal/server/api.go","line":40,"snippet":"q := r.URL\n","sha":"`+sha1+`"}, "internal/store/items.go:120-134"]}]}`), &in); err != nil {
		t.Fatal(err)
	}
	items := must(s.BulkCreate("me", "API", in))
	it := must(s.GetItem(items[0].Ref))
	if len(it.Locations) != 2 || it.Locations[0] != (Location{Path: "internal/store/items.go", Line: 120, EndLine: 134}) {
		t.Fatalf("locations %+v", it.Locations)
	}
	if l := it.Locations[1]; l.Path != "internal/server/api.go" || l.Snippet != "q := r.URL" || l.SHA != sha1 {
		t.Fatalf("second %+v", l)
	}
	// Locations and recorded changes live side by side.
	must(s.RecordChanges("me", it.Ref, Changes{Files: []ChangedFile{{Path: "internal/store/items.go", Added: 3}}}))
	it = must(s.GetItem(it.Ref))
	if len(it.Locations) != 2 || it.ChangedFiles != 1 {
		t.Fatalf("after changes: %d locations, %d files", len(it.Locations), it.ChangedFiles)
	}
	it = must(s.UpdateItem("me", it.Ref, ItemPatch{Locations: &[]Location{{Path: "b.go", Line: 1}}}))
	if len(it.Locations) != 1 || it.Locations[0].Path != "b.go" || it.ChangedFiles != 1 {
		t.Fatalf("replace %+v", it.Locations)
	}
	it = must(s.UpdateItem("me", it.Ref, ItemPatch{Locations: &[]Location{}}))
	if len(it.Locations) != 0 {
		t.Fatalf("clear %+v", it.Locations)
	}
	for _, bad := range []Location{{Path: ""}, {Path: "/abs.go"}, {Path: "a.go", Line: 9, EndLine: 3}, {Path: "a.go", EndLine: 3}, {Path: "a.go", SHA: "xyz"}} {
		if _, err := s.UpdateItem("me", it.Ref, ItemPatch{Locations: &[]Location{bad}}); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted %+v: %v", bad, err)
		}
	}
	if _, err := s.BulkUpdate("me", "API", []string{it.Ref}, ItemPatch{Locations: &[]Location{}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bulk update set locations: %v", err)
	}
}
