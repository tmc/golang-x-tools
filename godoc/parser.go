// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file contains support functions for parsing .go files
// accessed via godoc's file system fs.

package godoc

import (
	"bytes"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	pathpkg "path"

	"golang.org/x/tools/godoc/vfs"
)

var linePrefix = []byte("//line ")

// This function replaces source lines starting with "//line " with a blank line.
// It does this irrespective of whether the line is truly a line comment or not;
// e.g., the line may be inside a string, or a /*-style comment; however that is
// rather unlikely (proper testing would require a full Go scan which we want to
// avoid for performance).
func replaceLinePrefixCommentsWithBlankLine(src []byte) {
	for {
		i := bytes.Index(src, linePrefix)
		if i < 0 {
			break // we're done
		}
		// 0 <= i && i+len(linePrefix) <= len(src)
		if i == 0 || src[i-1] == '\n' {
			// at beginning of line: blank out line
			for i < len(src) && src[i] != '\n' {
				src[i] = ' '
				i++
			}
		} else {
			// not at beginning of line: skip over prefix
			i += len(linePrefix)
		}
		// i <= len(src)
		src = src[i:]
	}
}

func (c *Corpus) parseFile(fset *token.FileSet, filename string, mode parser.Mode) (*ast.File, error) {
	src, err := vfs.ReadFile(c.fs, filename)
	if err != nil {
		return nil, err
	}

	// Temporary ad-hoc fix for issue 5247.
	// TODO(gri,dmitshur) Remove this in favor of a better fix, eventually (see issue 32092).
	replaceLinePrefixCommentsWithBlankLine(src)

	return parser.ParseFile(fset, filename, src, mode)
}

// shouldIncludeFile checks build constraints in file content to determine
// if it should be included for the given build tags.
func shouldIncludeFile(filename string, content []byte, buildTags []string) bool {
	// First try with the standard method
	ctx := build.Default

	// Deep copy the build tags to avoid modifying the original slice
	if len(buildTags) > 0 {
		tags := make([]string, len(buildTags))
		copy(tags, buildTags)
		ctx.BuildTags = tags
	}

	// Create a match context for evaluation
	match, err := ctx.MatchFile(filename, string(content))
	if err != nil {
		// If there's an error determining the match, conservatively include the file
		return true
	}
	
	// Handle special case for files with build tags since the standard build
	// context might not report them correctly in some cases
	if !match && len(buildTags) > 0 {
		// Look for explicit build constraint matching our tags
		for _, tag := range buildTags {
			if bytes.Contains(content, []byte("// +build "+tag)) || 
			   bytes.Contains(content, []byte("//go:build "+tag)) {
				return true
			}
		}
	}
	
	return match
}

// parseFileWithBuildTags parses the named file with respect to the specified build tags.
// If no build tags are provided, it behaves the same as parseFile.
func (c *Corpus) parseFileWithBuildTags(fset *token.FileSet, filename string, mode parser.Mode, buildTags []string) (*ast.File, error) {
	src, err := vfs.ReadFile(c.fs, filename)
	if err != nil {
		return nil, err
	}

	// Temporary ad-hoc fix for issue 5247.
	// TODO(gri,dmitshur) Remove this in favor of a better fix, eventually (see issue 32092).
	replaceLinePrefixCommentsWithBlankLine(src)

	// When build tags are specified, we need to check if the file should be included
	if len(buildTags) > 0 {
		// Check if the file should be included based on build constraints
		if !shouldIncludeFile(filename, src, buildTags) {
			// Return nil if the file should be excluded based on build constraints
			return nil, nil
		}
	}

	return parser.ParseFile(fset, filename, src, mode)
}

func (c *Corpus) parseFiles(fset *token.FileSet, relpath string, abspath string, localnames []string) (map[string]*ast.File, error) {
	return c.parseFilesWithBuildTags(fset, relpath, abspath, localnames, nil)
}

// parseFilesWithBuildTags is like parseFiles but respects build tags when parsing files.
func (c *Corpus) parseFilesWithBuildTags(fset *token.FileSet, relpath string, abspath string, localnames []string, buildTags []string) (map[string]*ast.File, error) {
	files := make(map[string]*ast.File)

	for _, f := range localnames {
		absname := pathpkg.Join(abspath, f)
		var file *ast.File
		var err error

		if len(buildTags) > 0 {
			file, err = c.parseFileWithBuildTags(fset, absname, parser.ParseComments, buildTags)
		} else {
			file, err = c.parseFile(fset, absname, parser.ParseComments)
		}

		if err != nil {
			return nil, err
		}

		// Skip files that were excluded by build constraints
		if file == nil {
			continue
		}

		files[pathpkg.Join(relpath, f)] = file
	}

	return files, nil
}
