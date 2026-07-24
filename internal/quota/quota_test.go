package quota

import (
	"context"
	"path/filepath"
	"testing"
)

func TestHelperRejectsArbitraryPathAndDirectory(t *testing.T) {
	root := t.TempDir()
	helper := NewHelper(root)
	helper.run = func(context.Context, string, ...string) error { return nil }
	helper.Path = "/tmp/attacker-helper"
	if err := helper.Validate(context.Background()); err == nil {
		t.Fatal("arbitrary helper path was accepted")
	}
	helper.Path = HelperPath
	if err := helper.Apply(context.Background(), filepath.Join(root, "..", "etc"), 1); err == nil {
		t.Fatal("arbitrary quota directory was accepted")
	}
	if err := helper.Apply(context.Background(), filepath.Join(root, "service_1", "nested"), 1); err == nil {
		t.Fatal("nested quota directory was accepted")
	}
}

func TestHelperUsesFixedArguments(t *testing.T) {
	root := t.TempDir()
	helper := NewHelper(root)
	var got []string
	helper.run = func(_ context.Context, path string, args ...string) error {
		got = append([]string{path}, args...)
		return nil
	}
	directory := filepath.Join(root, "service_1")
	if err := helper.Apply(context.Background(), directory, 32); err != nil {
		t.Fatal(err)
	}
	want := []string{HelperPath, "apply", "--directory", directory, "--gib", "32"}
	if len(got) != len(want) {
		t.Fatalf("arguments = %v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("arguments = %v, want %v", got, want)
		}
	}
}
