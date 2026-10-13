package encode

import (
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
)

// VideoExts are the file extensions treated as video.
var VideoExts = []string{
	".3gp", ".avi", ".flv", ".m2ts", ".m4v", ".mkv", ".mov", ".mp4", ".mpeg",
	".mpg", ".mts", ".ogv", ".ts", ".vob", ".webm", ".wmv", ".y4m",
}

// IsVideo reports whether path looks like a video file.
func IsVideo(path string) bool {
	return slices.Contains(VideoExts, strings.ToLower(filepath.Ext(path)))
}

// FindVideos returns every video under root, including in subfolders, in
// sorted order. Hidden files and folders are skipped, which includes results
// still being written.
func FindVideos(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil // skip unreadable folders rather than giving up
		}
		hidden := path != root && strings.HasPrefix(d.Name(), ".")
		switch {
		case d.IsDir() && hidden:
			return filepath.SkipDir
		case !d.IsDir() && !hidden && IsVideo(path):
			out = append(out, path)
		}
		return nil
	})
	return out, err
}
