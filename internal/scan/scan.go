// Package scan finds video files in the library so they can be enqueued.
package scan

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
)

var videoExtensions = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".ts": true, ".m2ts": true, ".wmv": true,
}

func IsVideo(path string) bool {
	return videoExtensions[strings.ToLower(filepath.Ext(path))]
}

// Walk calls visit for every video file under each root. Unreadable entries
// don't stop the sweep; they're collected and reported at the end.
func Walk(roots []string, visit func(path string) error) error {
	var problems []error
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				problems = append(problems, err)
				return nil
			}
			if entry.IsDir() || !IsVideo(path) {
				return nil
			}
			if err := visit(path); err != nil {
				problems = append(problems, err)
			}
			return nil
		})
		if err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

// Collect gathers every video file under each root into one slice, for
// callers that need the total count up front (e.g. to show "3 of 20").
func Collect(roots []string) ([]string, error) {
	var files []string
	err := Walk(roots, func(path string) error {
		files = append(files, path)
		return nil
	})
	return files, err
}
