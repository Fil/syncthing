package model

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

func loadIgnoreFile(ignFile, base string, filesSeen map[string]bool) ([]*regexp.Regexp, error) {
	if filesSeen == nil {
		filesSeen = make(map[string]bool)
	} else {
		if filesSeen[ignFile] {
			return nil, fmt.Errorf("Multiple include of ignore file", ignFile)
		}
		filesSeen[ignFile] = true
	}

	fd, err := os.Open(ignFile)
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	return parseIgnoreFile(fd, base, ignFile, filesSeen)
}

func parseIgnoreFile(fd io.Reader, base, currentFile string, filesSeen map[string]bool) ([]*regexp.Regexp, error) {
	var exps []*regexp.Regexp
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
				l.Warnf("Invalid pattern %q in ignore file", line)
				continue
			}
			exps = append(exps, exp)
		} else if strings.HasPrefix(line, "**/") {
			// Add the pattern as is, and without **/ so it matches in current dir
			exp, err := fnmatch.Convert(line, fnmatch.FNM_PATHNAME)
			if err != nil {
				l.Warnf("Invalid pattern %q in ignore file", line)
				continue
			}
			exps = append(exps, exp)

			exp, err = fnmatch.Convert(path.Join(base, line[3:]), fnmatch.FNM_PATHNAME)
			if err != nil {
				l.Warnf("Invalid pattern %q in ignore file", line)
				continue
			}
			exps = append(exps, exp)
		} else if strings.HasPrefix(line, "#include ") {
			includeFile := filepath.Join(filepath.Dir(currentFile), line[len("#include "):])
			includes, err := loadIgnoreFile(includeFile, base, filesSeen)
			if err != nil {
				l.Warnln(err)
			} else {
				exps = append(exps, includes...)
			}
		} else {
			// Path name or pattern, add it so it matches files both in
			// current directory and subdirs.
			exp, err := fnmatch.Convert(path.Join(base, line), fnmatch.FNM_PATHNAME)
			if err != nil {
				l.Warnf("Invalid pattern %q in ignore file", line)
				continue
			}
			exps = append(exps, exp)

			exp, err = fnmatch.Convert(path.Join(base, "**", line), fnmatch.FNM_PATHNAME)
			if err != nil {
				l.Warnf("Invalid pattern %q in ignore file", line)
				continue
			}
			exps = append(exps, exp)
		}
	}

	return exps, nil
}

func shouldIgnore(patterns []*regexp.Regexp, file string) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(file) {
			return true
		}
	}
	return false
}
