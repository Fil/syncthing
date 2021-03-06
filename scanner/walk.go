// Copyright (C) 2014 Jakob Borg and Contributors (see the CONTRIBUTORS file).
// All rights reserved. Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package scanner

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"code.google.com/p/go.text/unicode/norm"

	"github.com/syncthing/syncthing/fnmatch"
	"github.com/syncthing/syncthing/lamport"
	"github.com/syncthing/syncthing/protocol"
)

type Walker struct {
	// Dir is the base directory for the walk
	Dir string
	// Limit walking to this path within Dir, or no limit if Sub is blank
	Sub string
	// BlockSize controls the size of the block used when hashing.
	BlockSize int
	// If IgnoreFile is not empty, it is the name used for the file that holds ignore patterns.
	IgnoreFile string
	// If TempNamer is not nil, it is used to ignore tempory files when walking.
	TempNamer TempNamer
	// If CurrentFiler is not nil, it is queried for the current file before rescanning.
	CurrentFiler CurrentFiler
	// If IgnorePerms is true, changes to permission bits will not be
	// detected. Scanned files will get zero permission bits and the
	// NoPermissionBits flag set.
	IgnorePerms bool
}

type TempNamer interface {
	// Temporary returns a temporary name for the filed referred to by filepath.
	TempName(path string) string
	// IsTemporary returns true if path refers to the name of temporary file.
	IsTemporary(path string) bool
}

type CurrentFiler interface {
	// CurrentFile returns the file as seen at last scan.
	CurrentFile(name string) protocol.FileInfo
}

// Walk returns the list of files found in the local repository by scanning the
// file system. Files are blockwise hashed.
func (w *Walker) Walk() (chan protocol.FileInfo, error) {
	if debug {
		l.Debugln("Walk", w.Dir, w.Sub, w.BlockSize, w.IgnoreFile)
	}

	err := checkDir(w.Dir)
	if err != nil {
		return nil, err
	}

	files := make(chan protocol.FileInfo)
	hashedFiles := make(chan protocol.FileInfo)
	newParallelHasher(w.Dir, w.BlockSize, runtime.NumCPU(), hashedFiles, files)

	var ignores []*regexp.Regexp
	go func() {
		filepath.Walk(w.Dir, w.loadIgnoreFiles(w.Dir, &ignores))

		hashFiles := w.walkAndHashFiles(files, ignores)
		filepath.Walk(filepath.Join(w.Dir, w.Sub), hashFiles)
		close(files)
	}()

	return hashedFiles, nil
}

// CleanTempFiles removes all files that match the temporary filename pattern.
func (w *Walker) CleanTempFiles() {
	filepath.Walk(w.Dir, w.cleanTempFile)
}

func (w *Walker) loadIgnoreFiles(dir string, ignores *[]*regexp.Regexp) filepath.WalkFunc {
	return func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		rn, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}

		if pn, sn := filepath.Split(rn); sn == w.IgnoreFile {
			pn := filepath.Clean(pn)
			filesSeen := make(map[string]map[string]bool)
			dirIgnores := loadIgnoreFile(p, pn, filesSeen)
			*ignores = append(*ignores, dirIgnores...)
		}

		return nil
	}
}

func loadIgnoreFile(ignFile, base string, filesSeen map[string]map[string]bool) []*regexp.Regexp {
	fd, err := os.Open(ignFile)
	if err != nil {
		return nil
	}
	defer fd.Close()
	return parseIgnoreFile(fd, base, ignFile, filesSeen)
}

func parseIgnoreFile(fd io.Reader, base, currentFile string, filesSeen map[string]map[string]bool) []*regexp.Regexp {
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
			includeFile := filepath.Join(filepath.Dir(currentFile), strings.Replace(line, "#include ", "", 1))
			if _, err := os.Stat(includeFile); os.IsNotExist(err) {
				l.Infoln("Could not open ignore include file", includeFile)
			} else {
				seen := false
				if seenByCurrent, ok := filesSeen[currentFile]; ok {
					_, seen = seenByCurrent[includeFile]
				}

				if seen {
					l.Warnf("Recursion detected while including %s from %s", includeFile, currentFile)
				} else {
					if filesSeen[currentFile] == nil {
						filesSeen[currentFile] = make(map[string]bool)
					}
					filesSeen[currentFile][includeFile] = true
					includes := loadIgnoreFile(includeFile, base, filesSeen)
					exps = append(exps, includes...)
				}
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

	return exps
}

func (w *Walker) walkAndHashFiles(fchan chan protocol.FileInfo, ignores []*regexp.Regexp) filepath.WalkFunc {
	return func(p string, info os.FileInfo, err error) error {
		if err != nil {
			if debug {
				l.Debugln("error:", p, info, err)
			}
			return nil
		}

		rn, err := filepath.Rel(w.Dir, p)
		if err != nil {
			if debug {
				l.Debugln("rel error:", p, err)
			}
			return nil
		}

		if rn == "." {
			return nil
		}

		if w.TempNamer != nil && w.TempNamer.IsTemporary(rn) {
			// A temporary file
			if debug {
				l.Debugln("temporary:", rn)
			}
			return nil
		}

		if sn := filepath.Base(rn); sn == w.IgnoreFile || sn == ".stversions" || w.ignoreFile(ignores, rn) {
			// An ignored file
			if debug {
				l.Debugln("ignored:", rn)
			}
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if (runtime.GOOS == "linux" || runtime.GOOS == "windows") && !norm.NFC.IsNormalString(rn) {
			l.Warnf("File %q contains non-NFC UTF-8 sequences and cannot be synced. Consider renaming.", rn)
			return nil
		}

		if info.Mode().IsDir() {
			if w.CurrentFiler != nil {
				cf := w.CurrentFiler.CurrentFile(rn)
				permUnchanged := w.IgnorePerms || !protocol.HasPermissionBits(cf.Flags) || PermsEqual(cf.Flags, uint32(info.Mode()))
				if !protocol.IsDeleted(cf.Flags) && protocol.IsDirectory(cf.Flags) && permUnchanged {
					return nil
				}
			}

			var flags uint32 = protocol.FlagDirectory
			if w.IgnorePerms {
				flags |= protocol.FlagNoPermBits | 0777
			} else {
				flags |= uint32(info.Mode() & os.ModePerm)
			}
			f := protocol.FileInfo{
				Name:     rn,
				Version:  lamport.Default.Tick(0),
				Flags:    flags,
				Modified: info.ModTime().Unix(),
			}
			if debug {
				l.Debugln("dir:", f)
			}
			fchan <- f
			return nil
		}

		if info.Mode().IsRegular() {
			if w.CurrentFiler != nil {
				cf := w.CurrentFiler.CurrentFile(rn)
				permUnchanged := w.IgnorePerms || !protocol.HasPermissionBits(cf.Flags) || PermsEqual(cf.Flags, uint32(info.Mode()))
				if !protocol.IsDeleted(cf.Flags) && cf.Modified == info.ModTime().Unix() && permUnchanged {
					return nil
				}

				if debug {
					l.Debugln("rescan:", cf, info.ModTime().Unix(), info.Mode()&os.ModePerm)
				}
			}

			var flags = uint32(info.Mode() & os.ModePerm)
			if w.IgnorePerms {
				flags = protocol.FlagNoPermBits | 0666
			}

			fchan <- protocol.FileInfo{
				Name:     rn,
				Version:  lamport.Default.Tick(0),
				Flags:    flags,
				Modified: info.ModTime().Unix(),
			}
		}

		return nil
	}
}

func (w *Walker) cleanTempFile(path string, info os.FileInfo, err error) error {
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeType == 0 && w.TempNamer.IsTemporary(path) {
		os.Remove(path)
	}
	return nil
}

func (w *Walker) ignoreFile(patterns []*regexp.Regexp, file string) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(file) {
			if debug {
				l.Debugf("%q matches %v", file, pattern)
			}
			return true
		}
	}
	return false
}

func checkDir(dir string) error {
	if info, err := os.Lstat(dir); err != nil {
		return err
	} else if !info.IsDir() {
		return errors.New(dir + ": not a directory")
	} else if debug {
		l.Debugln("checkDir", dir, info)
	}
	return nil
}

func PermsEqual(a, b uint32) bool {
	switch runtime.GOOS {
	case "windows":
		// There is only writeable and read only, represented for user, group
		// and other equally. We only compare against user.
		return a&0600 == b&0600
	default:
		// All bits count
		return a&0777 == b&0777
	}
}
