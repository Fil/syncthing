// Copyright (C) 2014 Jakob Borg and Contributors (see the CONTRIBUTORS file).
// All rights reserved. Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ignore

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/syncthing/syncthing/fnmatch"
)

type Patterns []*regexp.Regexp

func Load(file string) (Patterns, error) {
	base := filepath.Dir(file)
	seen := make(map[string]bool)
	return loadIgnoreFile(file, base, seen)
}

func (l Patterns) Match(file string) bool {
	for _, pattern := range l {
		if pattern.MatchString(file) {
			return true
		}
	}
	return false
}

func loadIgnoreFile(file, base string, seen map[string]bool) (Patterns, error) {

	if seen[file] {
		return nil, fmt.Errorf("Multiple include of ignore file %q", file)
	}
	seen[file] = true

	fd, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	return parseIgnoreFile(fd, base, file, seen)
}

func parseIgnoreFile(fd io.Reader, base, currentFile string, seen map[string]bool) (Patterns, error) {
	var exps Patterns
	scanner := bufio.NewScanner(fd)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "/") {
			// Pattern is rooted in the current dir only
			exp, err := fnmatch.Convert(path.Join(base, line[1:]), fnmatch.FNM_PATHNAME)
			if err != nil {
				return nil, fmt.Errorf("Invalid pattern %q in ignore file", line)
			}
			exps = append(exps, exp)
		} else if strings.HasPrefix(line, "**/") {
			// Add the pattern as is, and without **/ so it matches in current dir
			exp, err := fnmatch.Convert(line, fnmatch.FNM_PATHNAME)
			if err != nil {
				return nil, fmt.Errorf("Invalid pattern %q in ignore file", line)
			}
			exps = append(exps, exp)

			exp, err = fnmatch.Convert(path.Join(base, line[3:]), fnmatch.FNM_PATHNAME)
			if err != nil {
				return nil, fmt.Errorf("Invalid pattern %q in ignore file", line)
			}
			exps = append(exps, exp)
		} else if strings.HasPrefix(line, "#include ") {
			includeFile := filepath.Join(filepath.Dir(currentFile), line[len("#include "):])
			includes, err := loadIgnoreFile(includeFile, base, seen)
			if err != nil {
				return nil, err
			} else {
				exps = append(exps, includes...)
			}
		} else {
			// Path name or pattern, add it so it matches files both in
			// current directory and subdirs.
			exp, err := fnmatch.Convert(path.Join(base, line), fnmatch.FNM_PATHNAME)
			if err != nil {
				return nil, fmt.Errorf("Invalid pattern %q in ignore file", line)
			}
			exps = append(exps, exp)

			exp, err = fnmatch.Convert(path.Join(base, "**", line), fnmatch.FNM_PATHNAME)
			if err != nil {
				return nil, fmt.Errorf("Invalid pattern %q in ignore file", line)
			}
			exps = append(exps, exp)
		}
	}

	return exps, nil
}
