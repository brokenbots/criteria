package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeFloors(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "floors.txt")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadFloors(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    []floor
		wantErr bool
	}{
		{
			name: "valid",
			body: "# comment\n\ninternal/cli 62.5\nworkflow 84.5\n",
			want: []floor{{"internal/cli", 62.5}, {"workflow", 84.5}},
		},
		{
			name: "blank and comments ignored",
			body: "# header\ninternal/cli 62.5\n# inline-ish\n\nworkflow 84.5\n",
			want: []floor{{"internal/cli", 62.5}, {"workflow", 84.5}},
		},
		{
			name:    "malformed line",
			body:    "internal/cli\n",
			wantErr: true,
		},
		{
			name:    "bad value",
			body:    "internal/cli notanumber\n",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readFloors(writeFloors(t, tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCompareFloors(t *testing.T) {
	cases := []struct {
		name           string
		base           []floor
		head           []floor
		wantDecreases  []floorChange
		wantIncreases  []floorChange
		wantAdded      []string
		wantRemoved    []string
	}{
		{
			name: "no changes",
			base: []floor{{"a", 50.0}, {"b", 60.0}},
			head: []floor{{"a", 50.0}, {"b", 60.0}},
		},
		{
			name: "decrease triggers",
			base: []floor{{"a", 67.0}},
			head: []floor{{"a", 65.5}},
			wantDecreases: []floorChange{{"a", 67.0, 65.5}},
		},
		{
			name: "increase allowed",
			base: []floor{{"a", 50.0}},
			head: []floor{{"a", 55.0}},
			wantIncreases: []floorChange{{"a", 50.0, 55.0}},
		},
		{
			name: "decrease and increase together",
			base: []floor{{"a", 67.0}, {"b", 50.0}},
			head: []floor{{"a", 65.5}, {"b", 55.0}},
			wantDecreases: []floorChange{{"a", 67.0, 65.5}},
			wantIncreases: []floorChange{{"b", 50.0, 55.0}},
		},
		{
			name:       "addition allowed",
			base: []floor{{"a", 50.0}},
			head: []floor{{"a", 50.0}, {"b", 60.0}},
			wantAdded:  []string{"b"},
		},
		{
			name:        "removal allowed",
			base: []floor{{"a", 50.0}, {"b", 60.0}},
			head:        []floor{{"a", 50.0}},
			wantRemoved: []string{"b"},
		},
		{
			name:          "new package decrease is an addition not a decrease",
			base: []floor{{"a", 50.0}},
			head: []floor{{"a", 50.0}, {"b", 40.0}},
			wantAdded:     []string{"b"},
		},
		{
			name: "floating tolerance unchanged",
			base: []floor{{"a", 50.0}},
			head: []floor{{"a", 50.0000000001}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := compareFloors(tc.base, tc.head)
			if !reflect.DeepEqual(got.decreases, tc.wantDecreases) {
				t.Errorf("decreases = %+v, want %+v", got.decreases, tc.wantDecreases)
			}
			if !reflect.DeepEqual(got.increases, tc.wantIncreases) {
				t.Errorf("increases = %+v, want %+v", got.increases, tc.wantIncreases)
			}
			if !reflect.DeepEqual(got.added, tc.wantAdded) {
				t.Errorf("added = %+v, want %+v", got.added, tc.wantAdded)
			}
			if !reflect.DeepEqual(got.removed, tc.wantRemoved) {
				t.Errorf("removed = %+v, want %+v", got.removed, tc.wantRemoved)
			}
		})
	}
}

func TestCompareFloors_Sorted(t *testing.T) {
	base := []floor{{"c", 70}, {"a", 60}, {"b", 80}}
	head := []floor{{"c", 65}, {"a", 75}, {"b", 80}}
	got := compareFloors(base, head)
	wantDecreases := []floorChange{{"c", 70, 65}}
	wantIncreases := []floorChange{{"a", 60, 75}}
	if !reflect.DeepEqual(got.decreases, wantDecreases) {
		t.Errorf("decreases = %+v, want %+v", got.decreases, wantDecreases)
	}
	if !reflect.DeepEqual(got.increases, wantIncreases) {
		t.Errorf("increases = %+v, want %+v", got.increases, wantIncreases)
	}
}

func TestReadFloors_MissingFile(t *testing.T) {
	if _, err := readFloors(filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Fatal("expected error for missing file")
	}
}
