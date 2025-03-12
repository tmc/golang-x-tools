// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package godoc

import (
	"testing"
)

// TestShouldIncludeFile tests the shouldIncludeFile function with various build constraints.
func TestShouldIncludeFile(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		buildTags []string
		want      bool
	}{
		{
			name: "old-style build tag - matching",
			content: `// +build foo

package main

func main() {}`,
			buildTags: []string{"foo"},
			want:      true,
		},
		{
			name: "old-style build tag - non-matching",
			content: `// +build bar

package main

func Bar() {}`,
			buildTags: []string{"foo"},
			want:      false,
		},
		{
			name: "new-style build tag - matching",
			content: `//go:build foo

package main

func main() {}`,
			buildTags: []string{"foo"},
			want:      true,
		},
		{
			name: "new-style build tag - non-matching",
			content: `//go:build bar

package main

func Bar() {}`,
			buildTags: []string{"foo"},
			want:      false,
		},
		{
			name: "mixed build tags - matching",
			content: `//go:build foo && !bar
// +build foo,!bar

package main

func main() {}`,
			buildTags: []string{"foo"},
			want:      true,
		},
		{
			name: "mixed build tags - non-matching",
			content: `//go:build bar && !foo
// +build bar,!foo

package main

func Bar() {}`,
			buildTags: []string{"foo"},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldIncludeFile("test.go", []byte(tt.content), tt.buildTags)
			if got != tt.want {
				t.Errorf("shouldIncludeFile() = %v, want %v", got, tt.want)
			}
		})
	}
}
